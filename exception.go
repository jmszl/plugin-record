package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/shirou/gopsutil/v3/disk"
	"net/http"
	"time"
)

// 向第三方发送异常报警
func SendToThirdPartyAPI(exception *Exception) {
	exception.CreateTime = time.Now().Format("2006-01-02 15:04:05")
	exception.ServerIP = RecordPluginConfig.LocalIp
	data, err := json.Marshal(exception)
	if err != nil {
		fmt.Println("Error marshalling exception:", err)
		return
	}
	err = db.Create(&exception).Error
	if err != nil {
		fmt.Println("异常数据插入数据库失败:", err)
		return
	}
	resp, err := http.Post(RecordPluginConfig.ExceptionPostUrl, "application/json", bytes.NewBuffer(data))
	if err != nil {
		fmt.Println("Error sending exception to third party API:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("Failed to send exception, status code:", resp.StatusCode)
	} else {
		fmt.Println("Exception sent successfully!")
	}
}

// 磁盘超上限报警
func getDisckException(streamPath string) bool {
	d, _ := disk.Usage("/")
	if d.UsedPercent >= RecordPluginConfig.DiskMaxPercent {
		exceptionChannel <- &Exception{AlarmType: "disk alarm", AlarmDesc: "disk is full", StreamPath: streamPath}
		return true
	}
	return false
}
