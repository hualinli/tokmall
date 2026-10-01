package mediaprobe

import (
	"encoding/binary"
	"io"
	"os"
)

// moovAtFront 判断 mp4 的 moov atom 是否在文件头部。
//
// 实现方式：顺序扫描顶层 box（每个 box = 4 字节长度 + 4 字节类型），
// 只要在遇到 mdat 之前先遇到 moov，就说明元数据在前、可以流式播放。
// 用真实的 box 解析而不是"读前 4KB 找 moov 字符串"：
// 后者会把 mdat 里的随机字节误判成 moov。
func moovAtFront(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	header := make([]byte, 8)
	for range 32 { // 顶层 box 数量有限，扫 32 个足够
		if _, err := io.ReadFull(file, header); err != nil {
			return false
		}
		size := binary.BigEndian.Uint32(header[0:4])
		boxType := string(header[4:8])
		switch boxType {
		case "moov":
			return true
		case "mdat":
			// mdat 在 moov 之前 → 元数据在尾部，需要 remux。
			return false
		}
		switch size {
		case 1:
			// 64 位长度：额外 8 字节
			if _, err := file.Seek(8, io.SeekCurrent); err != nil {
				return false
			}
		case 0:
			// 长度 0 表示"到文件末尾"，后面不会再有 moov。
			return false
		default:
			if _, err := file.Seek(int64(size)-8, io.SeekCurrent); err != nil {
				return false
			}
		}
	}
	return false
}
