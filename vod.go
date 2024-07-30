package record

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"m7s.live/engine/v4/codec"
	"m7s.live/engine/v4/util"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func ext(path string) string {
	for i := len(path) - 1; i >= 0 && path[i] != '/'; i-- {
		if path[i] == '.' {
			return path[i:]
		}
	}
	return ""
}

func (conf *RecordConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch ext(r.URL.Path) {
	case ".flv":
		conf.Flv.ServeHTTP(w, r)
	case ".mp4":
		conf.Mp4.ServeHTTP(w, r)
	case ".m3u8", ".ts":
		conf.Hls.ServeHTTP(w, r)
	case ".h264", ".h265":
		conf.Raw.ServeHTTP(w, r)
	}
}

func putFlvTimestamp(header []byte, timestamp uint32) {
	header[4] = byte(timestamp >> 16)
	header[5] = byte(timestamp >> 8)
	header[6] = byte(timestamp)
	header[7] = byte(timestamp >> 24)
}

func (conf *RecordConfig) Play_flv_(w http.ResponseWriter, r *http.Request) {
	streamPath := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/play/flv/"), ".flv")
	singleFile := filepath.Join(conf.Flv.Path, streamPath+".flv")
	query := r.URL.Query()
	startTimeStr := query.Get("start")
	s, err := strconv.Atoi(startTimeStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	startTime := time.UnixMilli(int64(s))
	speedStr := query.Get("speed")
	speed, err := strconv.ParseFloat(speedStr, 64)
	if err != nil {
		speed = 1
	}
	dir := filepath.Join(conf.Flv.Path, streamPath)
	if util.Exist(singleFile) {

	} else if util.Exist(dir) {
		var fileList []fs.FileInfo
		var found bool
		var offsetTime time.Duration
		var offsetTimestamp uint32
		var lastTimestamp uint32
		var start = time.Now()
		speedControl := func() {
			targetTime := time.Duration(float64(time.Since(start)) * speed)
			sleepTime := time.Duration(lastTimestamp)*time.Millisecond - targetTime
			//fmt.Println("sleepTime", sleepTime)
			if sleepTime > 0 {
				time.Sleep(sleepTime)
			}
		}
		err = filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
			if info.IsDir() || !strings.HasSuffix(info.Name(), ".flv") {
				return nil
			}
			modTime := info.ModTime()
			//fmt.Println(path, modTime, startTime, found)
			if !found {
				if modTime.After(startTime) {
					found = true
				} else {
					fileList = []fs.FileInfo{info}
					offsetTime = startTime.Sub(modTime)
					return nil
				}
			}
			fileList = append(fileList, info)
			return nil
		})
		if !found {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "video/x-flv")
		w.Header().Set("Transfer-Encoding", "identity")
		w.WriteHeader(http.StatusOK)
		var writer io.Writer = w
		if hijacker, ok := w.(http.Hijacker); ok && conf.WriteTimeout > 0 {
			conn, _, _ := hijacker.Hijack()
			conn.SetWriteDeadline(time.Now().Add(conf.WriteTimeout))
			writer = conn
		} else {
			w.(http.Flusher).Flush()
		}
		flvHead := make([]byte, 9+4)
		tagHead := make(util.Buffer, 11)
		//tagLen := make([]byte, 4)
		var init bool
		if offsetTime == 0 {
			init = true
		} else {
			offsetTimestamp = -uint32(offsetTime.Milliseconds())
		}
		for i, info := range fileList {
			if r.Context().Err() != nil {
				return
			}
			filePath := filepath.Join(dir, info.Name())
			fmt.Println("read", filePath)
			file, err := os.Open(filePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			reader := bufio.NewReader(file)
			if i == 0 {
				// 第一次写入头
				_, err = io.ReadFull(reader, flvHead)
				_, err = writer.Write(flvHead)
			} else {
				// 后面的头跳过
				_, err = reader.Discard(13)
				if !init {
					offsetTime = 0
				}
			}

			for err == nil {
				_, err = io.ReadFull(reader, tagHead)
				if err != nil {
					break
				}
				tmp := tagHead
				t := tmp.ReadByte()
				dataLen := tmp.ReadUint24()
				lastTimestamp = tmp.ReadUint24() | uint32(tmp.ReadByte())<<24
				//fmt.Println(lastTimestamp, tagHead)
				if init {
					if t == codec.FLV_TAG_TYPE_SCRIPT {
						_, err = reader.Discard(int(dataLen) + 4)
					} else {
						lastTimestamp += offsetTimestamp
						putFlvTimestamp(tagHead, lastTimestamp)
						_, err = writer.Write(tagHead)
						_, err = io.CopyN(writer, reader, int64(dataLen+4))
						speedControl()
					}
					continue
				}
				switch t {
				case codec.FLV_TAG_TYPE_SCRIPT:
					_, err = reader.Discard(int(dataLen) + 4)
				case codec.FLV_TAG_TYPE_AUDIO:
					if lastTimestamp == 0 {
						_, err = writer.Write(tagHead)
						_, err = io.CopyN(writer, reader, int64(dataLen+4))
					} else {
						_, err = reader.Discard(int(dataLen) + 4)
					}
				case codec.FLV_TAG_TYPE_VIDEO:
					if lastTimestamp == 0 {
						_, err = writer.Write(tagHead)
						_, err = io.CopyN(writer, reader, int64(dataLen+4))
					} else {
						if lastTimestamp > uint32(offsetTime.Milliseconds()) {
							data := make([]byte, dataLen+4)
							_, err = io.ReadFull(reader, data)
							frameType := (data[0] >> 4) & 0b0111
							idr := frameType == 1 || frameType == 4
							if idr {
								init = true
								//fmt.Println("init", lastTimestamp)
								putFlvTimestamp(tagHead, 0)
								_, err = writer.Write(tagHead)
								_, err = writer.Write(data)
							}
						} else {
							_, err = reader.Discard(int(dataLen) + 4)
						}
					}
				}
			}
			offsetTimestamp = lastTimestamp
			err = file.Close()
		}
	} else {
		http.NotFound(w, r)
		return
	}
}
