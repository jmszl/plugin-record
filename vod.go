package record

import (
	"bufio"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"m7s.live/engine/v4/codec"
	"m7s.live/engine/v4/util"
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
	endTimeStr := query.Get("end")
	//s, err := strconv.Atoi(startTimeStr)
	startTime, err := time.ParseInLocation("20060102150405", startTimeStr, time.Local)
	endTime, err := time.ParseInLocation("20060102150405", endTimeStr, time.Local)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//startTime := time.UnixMilli(int64(s))
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
			//fmt.Println("sleepTime", sleepTime, time.Since(start).Milliseconds(), lastTimestamp)
			if sleepTime > 0 {
				time.Sleep(sleepTime)
			}
		}
		err = filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
			if info.IsDir() || !strings.HasSuffix(info.Name(), ".flv") {
				return nil
			}
			modTime := info.ModTime()
			//tmp, _ := strconv.Atoi(strings.TrimSuffix(info.Name(), ".flv"))
			//fileStartTime := time.Unix(tmp, 10)
			if !found {
				if modTime.After(startTime) {
					found = true
					//fmt.Println(path, modTime, startTime, found)
				} else {
					fileList = []fs.FileInfo{info}
					offsetTime = startTime.Sub(modTime)
					//fmt.Println(path, modTime, startTime, found)
					return nil
				}
			}
			if modTime.After(endTime) {
				return nil
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
		var init, seqAudioWritten, seqVideoWritten bool
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
			plugin.Debug("read", zap.String("file", filePath))
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
					offsetTimestamp = 0
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
					if !seqAudioWritten {
						putFlvTimestamp(tagHead, 0)
						_, err = writer.Write(tagHead)
						_, err = io.CopyN(writer, reader, int64(dataLen+4))
						seqAudioWritten = true
					} else {
						_, err = reader.Discard(int(dataLen) + 4)
					}
				case codec.FLV_TAG_TYPE_VIDEO:
					if !seqVideoWritten {
						putFlvTimestamp(tagHead, 0)
						_, err = writer.Write(tagHead)
						_, err = io.CopyN(writer, reader, int64(dataLen+4))
						seqVideoWritten = true
					} else {
						if lastTimestamp >= uint32(offsetTime.Milliseconds()) {
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

func (conf *RecordConfig) Download_flv_(w http.ResponseWriter, r *http.Request) {
	streamPath := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/download/flv/"), ".flv")
	singleFile := filepath.Join(conf.Flv.Path, streamPath+".flv")
	query := r.URL.Query()
	//rangeStr := strings.Split(query.Get("range"), "-")
	//s, err := strconv.Atoi(rangeStr[0])
	startTimeStr := query.Get("start")
	endTimeStr := query.Get("end")
	startTime, err := time.ParseInLocation("20060102150405", startTimeStr, time.Local)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//startTime := time.UnixMilli(int64(s))
	//e, err := strconv.Atoi(rangeStr[1])
	endTime, err := time.ParseInLocation("20060102150405", endTimeStr, time.Local)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//endTime := time.UnixMilli(int64(e))
	timeRange := endTime.Sub(startTime)
	plugin.Info("download", zap.String("stream", streamPath), zap.Time("start", startTime), zap.Time("end", endTime))
	dir := filepath.Join(conf.Flv.Path, streamPath)
	if util.Exist(singleFile) {

	} else if util.Exist(dir) {
		var fileList []fs.FileInfo
		var found bool
		var startOffsetTime time.Duration
		err = filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
			if info.IsDir() || !strings.HasSuffix(info.Name(), ".flv") {
				return nil
			}
			modTime := info.ModTime()
			//tmp, _ := strconv.Atoi(strings.TrimSuffix(info.Name(), ".flv"))
			//fileStartTime := time.Unix(tmp, 10)
			if !found {
				if modTime.After(startTime) {
					found = true
					//fmt.Println(path, modTime, startTime, found)
				} else {
					fileList = []fs.FileInfo{info}
					startOffsetTime = startTime.Sub(modTime)
					//fmt.Println(path, modTime, startTime, found)
					return nil
				}
			}
			if modTime.After(endTime) {
				return fs.ErrInvalid
			}
			fileList = append(fileList, info)
			return nil
		})
		if !found {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "video/x-flv")
		w.Header().Set("Content-Disposition", "attachment")
		var writer io.Writer = w
		flvHead := make([]byte, 9+4)
		tagHead := make(util.Buffer, 11)
		var contentLength uint64

		var amf *util.AMF
		var metaData util.EcmaArray
		initMetaData := func(reader io.Reader, dataLen uint32) {
			data := make([]byte, dataLen+4)
			_, err = io.ReadFull(reader, data)
			amf = &util.AMF{
				Buffer: util.Buffer(data[1+2+len("onMetaData") : len(data)-4]),
			}
			var obj any
			obj, err = amf.Unmarshal()
			metaData = obj.(map[string]any)
		}
		var filepositions []uint64
		var times []float64
		for pass := 0; pass < 2; pass++ {
			offsetTime := startOffsetTime
			var offsetTimestamp, lastTimestamp uint32
			var init, seqAudioWritten, seqVideoWritten bool
			if pass == 1 {
				metaData["keyframes"] = map[string]any{
					"filepositions": filepositions,
					"times":         times,
				}
				amf.Marshals("onMetaData", metaData)
				offsetDelta := amf.Len() + 15
				offset := offsetDelta + len(flvHead)
				contentLength += uint64(offset)
				metaData["duration"] = times[len(times)-1]
				metaData["filesize"] = contentLength
				for i := range filepositions {
					filepositions[i] += uint64(offset)
				}
				metaData["keyframes"] = map[string]any{
					"filepositions": filepositions,
					"times":         times,
				}
				amf.Reset()
				amf.Marshals("onMetaData", metaData)
				plugin.Info("start download", zap.Any("metaData", metaData))
				w.Header().Set("Content-Length", strconv.FormatInt(int64(contentLength), 10))
				w.WriteHeader(http.StatusOK)
			}
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
				plugin.Debug("read", zap.String("file", filePath))
				file, err := os.Open(filePath)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				reader := bufio.NewReader(file)
				if i == 0 {
					_, err = io.ReadFull(reader, flvHead)
					if pass == 1 {
						// 第一次写入头
						_, err = writer.Write(flvHead)
						tagHead[0] = codec.FLV_TAG_TYPE_SCRIPT
						l := amf.Len()
						tagHead[1] = byte(l >> 16)
						tagHead[2] = byte(l >> 8)
						tagHead[3] = byte(l)
						putFlvTimestamp(tagHead, 0)
						writer.Write(tagHead)
						writer.Write(amf.Buffer)
						l += 11
						util.BigEndian.PutUint32(tagHead[:4], uint32(l))
						writer.Write(tagHead[:4])
					}
				} else {
					// 后面的头跳过
					_, err = reader.Discard(13)
					if !init {
						offsetTime = 0
						offsetTimestamp = 0
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
							if pass == 0 {
								initMetaData(reader, dataLen)
							} else {
								_, err = reader.Discard(int(dataLen) + 4)
							}
						} else {
							lastTimestamp += offsetTimestamp
							if lastTimestamp >= uint32(timeRange.Milliseconds()) {
								break
							}
							if pass == 0 {
								data := make([]byte, dataLen+4)
								_, err = io.ReadFull(reader, data)
								frameType := (data[0] >> 4) & 0b0111
								idr := frameType == 1 || frameType == 4
								if idr {
									filepositions = append(filepositions, contentLength)
									times = append(times, float64(lastTimestamp)/1000)
								}
								contentLength += uint64(11 + dataLen + 4)
							} else {
								//fmt.Println("write", lastTimestamp)
								putFlvTimestamp(tagHead, lastTimestamp)
								_, err = writer.Write(tagHead)
								_, err = io.CopyN(writer, reader, int64(dataLen+4))
							}
						}
						continue
					}

					switch t {
					case codec.FLV_TAG_TYPE_SCRIPT:
						if pass == 0 {
							initMetaData(reader, dataLen)
						} else {
							_, err = reader.Discard(int(dataLen) + 4)
						}
					case codec.FLV_TAG_TYPE_AUDIO:
						if !seqAudioWritten {
							if pass == 0 {
								contentLength += uint64(11 + dataLen + 4)
								_, err = reader.Discard(int(dataLen) + 4)
							} else {
								putFlvTimestamp(tagHead, 0)
								_, err = writer.Write(tagHead)
								_, err = io.CopyN(writer, reader, int64(dataLen+4))
							}
							seqAudioWritten = true
						} else {
							_, err = reader.Discard(int(dataLen) + 4)
						}
					case codec.FLV_TAG_TYPE_VIDEO:
						if !seqVideoWritten {
							if pass == 0 {
								contentLength += uint64(11 + dataLen + 4)
								_, err = reader.Discard(int(dataLen) + 4)
							} else {
								putFlvTimestamp(tagHead, 0)
								_, err = writer.Write(tagHead)
								_, err = io.CopyN(writer, reader, int64(dataLen+4))
							}
							seqVideoWritten = true
						} else {
							if lastTimestamp >= uint32(offsetTime.Milliseconds()) {
								data := make([]byte, dataLen+4)
								_, err = io.ReadFull(reader, data)
								frameType := (data[0] >> 4) & 0b0111
								idr := frameType == 1 || frameType == 4
								if idr {
									init = true
									plugin.Debug("init", zap.Uint32("lastTimestamp", lastTimestamp))
									if pass == 0 {
										filepositions = append(filepositions, contentLength)
										times = append(times, float64(lastTimestamp)/1000)
										contentLength += uint64(11 + dataLen + 4)
									} else {
										putFlvTimestamp(tagHead, 0)
										_, err = writer.Write(tagHead)
										_, err = writer.Write(data)
									}
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
		}
		plugin.Info("end download")
	} else {
		http.NotFound(w, r)
		return
	}
}
