package native

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func UnTar(src string, dest string) error {
	fmt.Println("src:", src, "dest:", dest)
	// 打开准备解压的 tar 包
	fr, err := os.Open(src)
	if err != nil {
		return err
	}
	defer fr.Close()

	// 将打开的文件先解压
	//gr, err := gzip.NewReader(fr)
	//if err != nil {
	//	return err
	//}
	//defer gr.Close()

	// 通过 gr 创建 tar.Reader
	tr := tar.NewReader(fr)
	// 现在已经获得了 tar.Reader 结构了，只需要循环里面的数据写入文件就可以了
	for {
		hdr, err := tr.Next()
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		case hdr == nil:
			continue
		}

		dstFileDir := filepath.Join(dest, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if b := ExistDir(dstFileDir); b {
				continue
			}
			if err := os.MkdirAll(dstFileDir, 0775); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := copyFile(dstFileDir, hdr, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupport tar %s Typeflag %v", src, hdr.Typeflag)
		}
	}
}

func copyFile(dstFileDir string, hdr *tar.Header, tr *tar.Reader) error {
	file, err := os.OpenFile(dstFileDir, os.O_CREATE|os.O_RDWR, os.FileMode(hdr.Mode))
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = io.Copy(file, tr); err != nil {
		return err
	}
	return nil
}

// 判断目录是否存在
func ExistDir(dirname string) bool {
	fi, err := os.Stat(dirname)
	return (err == nil || os.IsExist(err)) && fi.IsDir()
}
