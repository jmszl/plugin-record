package record

import (
	"errors"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"log"
	"reflect"
)

// var mysqldb *gorm.DB
var err error

var createDataBaseSql = `CREATE DATABASE IF NOT EXISTS m7srecord;`

var useDataBaseSql = `USE m7srecord;`

func initMysqlDB(MysqlDSN string) *gorm.DB {
	mysqldb, err := gorm.Open(mysql.Open(MysqlDSN), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	mysqldb.Exec(createDataBaseSql)
	mysqldb.Exec(useDataBaseSql)
	mysqldb.AutoMigrate(&EventRecord{})
	mysqldb.AutoMigrate(&Exception{})
	return mysqldb
}

func paginate[T any](mysqldb *gorm.DB, model T, pageNum, pageSize int, filters map[string]interface{}) ([]T, int64, error) {
	var results []T
	var totalCount int64

	// 计算偏移量
	offset := (pageNum - 1) * pageSize

	// 查询总记录数
	countQuery := mysqldb.Model(model)

	// 使用反射设置字段值
	modelValue := reflect.ValueOf(&model).Elem() // 获取结构体值
	modelType := modelValue.Type()

	for field, value := range filters {
		if valueStr, ok := value.(string); ok && valueStr != "" {
			if field == "startTime" {
				countQuery = countQuery.Where("create_time >= ?", valueStr)
			} else if field == "endTime" {
				countQuery = countQuery.Where("create_time <= ?", valueStr)
			} else {
				// 使用反射查找字段并设置值
				fieldName, err := findFieldByName(modelType, field)
				if err != nil {
					return nil, 0, err
				}

				// 设置字段值
				if modelField := modelValue.FieldByName(fieldName); modelField.IsValid() && modelField.CanSet() {
					modelField.Set(reflect.ValueOf(valueStr))
					countQuery = countQuery.Where(&model)
				} else {
					return nil, 0, errors.New("invalid field: " + field)
				}
			}
		}
	}

	result := countQuery.Count(&totalCount)
	if result.Error != nil {
		return nil, 0, result.Error
	}

	// 查询当前页的数据
	query := mysqldb.Model(model).Limit(pageSize).Offset(offset)

	for field, value := range filters {
		if valueStr, ok := value.(string); ok && valueStr != "" {
			if field == "startTime" {
				query = query.Where("create_time >= ?", valueStr)
			} else if field == "endTime" {
				query = query.Where("create_time <= ?", valueStr)
			} else {
				// 使用反射设置查询字段值
				fieldName, err := findFieldByName(modelType, field)
				if err != nil {
					return nil, 0, err
				}

				if modelField := modelValue.FieldByName(fieldName); modelField.IsValid() && modelField.CanSet() {
					modelField.Set(reflect.ValueOf(valueStr))
					query = query.Where(&model)
				} else {
					return nil, 0, errors.New("invalid field: " + field)
				}
			}
		}
	}

	result = query.Find(&results)
	if result.Error != nil {
		return nil, 0, result.Error
	}

	return results, totalCount, nil
}

// findFieldByName 查找结构体中的字段名
func findFieldByName(modelType reflect.Type, field string) (string, error) {
	for i := 0; i < modelType.NumField(); i++ {
		structField := modelType.Field(i)
		if structField.Tag.Get("json") == field || structField.Name == field {
			return structField.Name, nil
		}
	}
	return "", errors.New("field not found: " + field)
}
