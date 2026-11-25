package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	source := flag.String("source", "", "启动包目录")
	output := flag.String("output", "", "输出 ZIP 文件")
	compress := flag.Bool("compress", false, "压缩启动包文件")
	flag.Parse()
	if *source == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "请指定 -source 和 -output")
		os.Exit(1)
	}
	if err := bundle(*source, *output, *compress); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s\n", *output)
}

func bundle(source, output string, compress bool) error {
	file, err := os.CreateTemp(filepath.Dir(output), ".bundle-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writer := zip.NewWriter(file)
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// 保留缓存目录，不附带系统元数据、链接或根目录本身。
		if path == source || entry.Name() == ".DS_Store" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		// Go 的 ZIP UTF-8 标志确保 Windows 正确识别中文名称。
		header.Name, header.Method = filepath.ToSlash(name), zip.Store
		if entry.IsDir() {
			header.Name += "/"
		} else if compress {
			header.Method = zip.Deflate
		}
		target, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		// 目录条目不包含文件数据。
		if entry.IsDir() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(target, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeZipErr, closeFileErr := writer.Close(), file.Close()
	if err != nil {
		return err
	}
	if closeZipErr != nil {
		return closeZipErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	return os.Rename(file.Name(), output)
}
