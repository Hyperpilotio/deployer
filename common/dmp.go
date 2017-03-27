package common

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"

	"github.com/golang/glog"
)

// Store struct to file
func Store(path string, object interface{}) error {
	b := new(bytes.Buffer)
	enc := gob.NewEncoder(b)
	if err := enc.Encode(object); err != nil {
		return fmt.Errorf("Unable to encode object to byte: %s", err.Error())
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return fmt.Errorf("Unable to open file path with %s: %s", path, err.Error())
	}
	defer file.Close()

	n, err := file.Write(b.Bytes())
	if err != nil {
		return fmt.Errorf("Unable to write byte array to file: %s", err.Error())
	}
	glog.Infof("%d bytes successfully written to file: %s", n, path)

	return nil
}

// Load file to struct
func Load(path string, object interface{}) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("Unable to open file path with %s: %s", path, err.Error())
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	if err := dec.Decode(object); err != nil {
		return fmt.Errorf("Unable to decode file to struct: %s", err.Error())
	}

	return nil
}
