package mcp

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func programFileID(path string, _ os.FileInfo) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var id windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &id); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x-%x-%x", id.VolumeSerialNumber, id.FileIndexHigh, id.FileIndexLow), nil
}
