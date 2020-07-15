package full_sync

import (
	"sync"
	"time"
	"fmt"

	"nimo-shake/common"
	"nimo-shake/configure"
	"nimo-shake/filter"
	"nimo-shake/writer"

	LOG "github.com/vinllen/log4go"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/vinllen/mgo/bson"
	"github.com/vinllen/mgo"
	"github.com/aws/aws-sdk-go/aws"
)

func Start(dynamoSession *dynamodb.DynamoDB, w writer.Writer) {
	// fetch all tables
	LOG.Info("start fetching table list")
	tableList, err := utils.FetchTableList(dynamoSession)
	if err != nil {
		LOG.Crashf("fetch table list failed[%v]", err)
	}
	LOG.Info("finish fetching table list: %v", tableList)

	tableList = filter.FilterList(tableList)

	if err := checkTableExists(tableList, w); err != nil {
		LOG.Crashf("check table exists failed[%v]", err)
		return
	}

	LOG.Info("start syncing: %v", tableList)

	fullChan := make(chan string, len(tableList))
	for _, table := range tableList {
		fullChan <- table
	}

	var wg sync.WaitGroup
	wg.Add(len(tableList))
	for i := 0; i < int(conf.Options.FullConcurrency); i++ {
		go func(id int) {
			for {
				table, ok := <-fullChan
				if !ok {
					// chan closed
					break
				}

				ts := NewTableSyncer(id, table)
				if ts == nil {
					LOG.Crashf("tableSyncer[%v] create failed", id)
				}

				LOG.Info("tableSyncer[%v] starts sync table[%v]", id, table)
				ts.Sync()
				LOG.Info("tableSyncer[%v] finish sync table[%v]", id, table)
				ts.Close()

				wg.Done()
			}
		}(i)
	}

	wg.Wait()
	close(fullChan)

	LOG.Info("finish syncing all tables and indexes!")
}

func checkTableExists(tableList []string, w writer.Writer) error {
	LOG.Info("target.db.exist is set[%v]", conf.Options.TargetDBExist)
	switch conf.Options.TargetType {
	case utils.TargetTypeMongo:
		sess := w.GetSession().(*mgo.Session)

		now := time.Now().Format(utils.GolangSecurityTime)
		collections, err := sess.DB(conf.Options.Id).CollectionNames()
		if err != nil {
			return fmt.Errorf("get target collection names error[%v]", err)
		}

		collectionsMp := utils.StringListToMap(collections)
		for _, table := range tableList {
			// check exist on the target mongodb
			if _, ok := collectionsMp[table]; ok {
				// exist
				LOG.Info("table[%v] exists", table)
				if conf.Options.TargetDBExist == utils.TargetDBExistDrop {
					if err := sess.DB(conf.Options.Id).C(table).DropCollection(); err != nil {
						return fmt.Errorf("drop target collection[%v] failed[%v]", table, err)
					}
				} else if conf.Options.TargetDBExist == utils.TargetDBExistRename {
					fromCollection := fmt.Sprintf("%s.%s", conf.Options.Id, table)
					toCollection := fmt.Sprintf("%s.%s_%v", conf.Options.Id, table, now)
					if err := sess.DB("admin").Run(bson.D{
						bson.DocElem{"renameCollection", fromCollection},
						bson.DocElem{"to", toCollection},
						bson.DocElem{"dropTarget", false},
					}, nil); err != nil {
						return fmt.Errorf("rename target collection[%v] failed[%v]", table, err)
					}
				} else {
					return fmt.Errorf("collection[%v] exists on the target", table)
				}
			}
		}
	case utils.TargetTypeAliyunDynamoProxy:
		sess := w.GetSession().(*dynamodb.DynamoDB)

		// query table list
		collections := make([]string, 0, 16)

		// dynamo-proxy is not support Limit and ExclusiveStartTableName
		/*lastTableName := aws.String("")
		var count int64 = 100
		for i := 0; ; i++ {
			LOG.Debug("list table round[%v]", i)
			var input *dynamodb.ListTablesInput
			if i == 0 {
				input = &dynamodb.ListTablesInput{
					Limit: aws.Int64(count),
				}
			} else {
				input = &dynamodb.ListTablesInput{
					ExclusiveStartTableName: lastTableName,
					Limit: aws.Int64(count),
				}
			}
			out, err := sess.ListTables(input)
			if err != nil {
				return fmt.Errorf("list table failed: %v", err)
			}

			for _, collection := range out.TableNames {
				collections = append(collections, *collection)
			}

			lastTableName = out.LastEvaluatedTableName
			if len(out.TableNames) < int(count) {
				break
			}
		}*/
		out, err := sess.ListTables(&dynamodb.ListTablesInput{})
		if err != nil {
			return fmt.Errorf("list table failed: %v", err)
		}
		for _, collection := range out.TableNames {
			collections = append(collections, *collection)
		}

		collectionsMp := utils.StringListToMap(collections)
		LOG.Info("target exit db list: %v", collections)
		for _, table := range tableList {
			// check exist on the target
			if _, ok := collectionsMp[table]; ok {
				// exist
				LOG.Info("table[%v] exists, try [%v]", table, conf.Options.TargetDBExist)
				if conf.Options.TargetDBExist == utils.TargetDBExistDrop {
					if _, err := sess.DeleteTable(&dynamodb.DeleteTableInput{
						TableName: aws.String(table),
					}); err != nil {
						return fmt.Errorf("drop target collection[%v] failed[%v]", table, err)
					}
				} else {
					return fmt.Errorf("collection[%v] exists on the target", table)
				}
			}
		}
	}

	LOG.Info("finish handling table exists")

	return nil
}
