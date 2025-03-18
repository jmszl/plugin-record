package record

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"log"
)

// sqlite数据库初始化，用来存放视频的关键帧等信息
func initSqliteDB(sqliteDbPath string) *gorm.DB {
	// 打开数据库连接
	sqlitedb, err := gorm.Open(sqlite.Open(sqliteDbPath), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	err = sqlitedb.AutoMigrate(&FLVKeyframe{})
	err = sqlitedb.AutoMigrate(&EventRecord{})
	err = sqlitedb.AutoMigrate(&Exception{})
	if err != nil {
		log.Fatal(err)
	}
	return sqlitedb
}
