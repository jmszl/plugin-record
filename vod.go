package record

import (
	"bufio"
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
			if sleepTime > 0 {
				time.Sleep(sleepTime)
			}
		}
		filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
			if info.IsDir() {
				return nil
			}
			modTime := info.ModTime()
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
		if len(fileList) == 0 {
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
		info := fileList[0]
		filePath := filepath.Join(dir, info.Name())
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reader := bufio.NewReader(file)
		flvHead := make([]byte, 9)
		tagHead := make(util.Buffer, 11)
		tagLen := make([]byte, 4)
		reader.Read(flvHead)
		writer.Write(flvHead)
		var init bool
		if offsetTime == 0 {
			init = true
		}

		for {
			_, err = reader.Read(tagHead)
			if err != nil {
				break
			}
			tmp := tagHead
			t := tmp.ReadByte()
			dataLen := tmp.ReadUint24()
			if init {
				if t == codec.FLV_TAG_TYPE_SCRIPT {
					reader.Discard(int(dataLen) + 4)
				} else {
					lastTimestamp = (tmp.ReadUint24() | uint32(tmp.ReadByte())<<24) - uint32(offsetTime.Milliseconds())
					tagHead[4] = byte(lastTimestamp >> 16)
					tagHead[5] = byte(lastTimestamp >> 8)
					tagHead[6] = byte(lastTimestamp)
					tagHead[7] = byte(lastTimestamp >> 24)
					writer.Write(tagHead)
					io.CopyN(writer, reader, int64(dataLen+4))
					speedControl()
				}
				continue
			}
			lastTimestamp = tmp.ReadUint24() | uint32(tmp.ReadByte())<<24
			switch t {
			case codec.FLV_TAG_TYPE_SCRIPT:
				reader.Discard(int(dataLen) + 4)
			case codec.FLV_TAG_TYPE_AUDIO:
				if lastTimestamp == 0 {
					writer.Write(tagHead)
					io.CopyN(writer, reader, int64(dataLen+4))
				} else {
					reader.Discard(int(dataLen) + 4)
				}
			case codec.FLV_TAG_TYPE_VIDEO:
				if lastTimestamp == 0 {
					writer.Write(tagHead)
					io.CopyN(writer, reader, int64(dataLen+4))
				} else {
					if lastTimestamp > uint32(offsetTime.Milliseconds()) {
						data := make([]byte, dataLen)
						reader.Read(data)
						reader.Read(tagLen)
						//isExtHeader := (b >> 4) & 0b1000
						frameType := (data[0] >> 4) & 0b0111
						idr := frameType == 1 || frameType == 4
						if idr {
							init = true
							tagHead[4] = 0
							tagHead[5] = 0
							tagHead[6] = 0
							tagHead[7] = 0
							writer.Write(tagHead)
							writer.Write(data)
							writer.Write(tagLen)
						}
					} else {
						reader.Discard(int(dataLen) + 4)
					}
				}
			}
		}
		file.Close()
		offsetTimestamp = lastTimestamp
		for _, info = range fileList[1:] {
			filePath = filepath.Join(dir, info.Name())
			file, err = os.Open(filePath)
			if err != nil {
				return
			}
			reader = bufio.NewReader(file)
			reader.Read(flvHead) // 跳过 flv 头
			for {
				_, err = reader.Read(tagHead)
				if err != nil {
					break
				}
				tmp := tagHead
				t := tmp.ReadByte()
				dataLen := tmp.ReadUint24()
				lastTimestamp = offsetTimestamp + (tmp.ReadUint24() | uint32(tmp.ReadByte())<<24) // 改写时间戳
				tagHead[4] = byte(lastTimestamp >> 16)
				tagHead[5] = byte(lastTimestamp >> 8)
				tagHead[6] = byte(lastTimestamp)
				tagHead[7] = byte(lastTimestamp >> 24)
				if init {
					if t == codec.FLV_TAG_TYPE_SCRIPT {
						reader.Discard(int(dataLen) + 4)
					} else {
						writer.Write(tagHead)
						io.CopyN(writer, reader, int64(dataLen+4))
						speedControl()
					}
					continue
				}
			}
			file.Close()
			offsetTimestamp = lastTimestamp
		}
	} else {
		http.NotFound(w, r)
		return
	}
}
