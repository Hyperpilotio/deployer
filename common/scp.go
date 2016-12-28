package common

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"path"

	"github.com/golang/glog"
)

type ScpClient struct {
	Host         string
	ClientConfig *ssh.ClientConfig
	Client       *ssh.Client
}

// Connects to the remote SSH server, returns error if it couldn't establish a session to the SSH server
func (a *ScpClient) Connect() error {
	client, err := ssh.Dial("tcp", a.Host, a.ClientConfig)
	if err != nil {
		return err
	}

	a.Client = client

	return nil
}

// Copies the contents of an io.Reader to a remote location
func (a *ScpClient) CopyFile(fileReader io.Reader, remotePath string, permissions string) error {
	contents_bytes, _ := ioutil.ReadAll(fileReader)
	contents := string(contents_bytes)
	filename := path.Base(remotePath)
	directory := path.Dir(remotePath)

	session, err := a.Client.NewSession()
	if err != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}
	session.Run("mkdir -p " + directory)

	session, err = a.Client.NewSession()
	var stderrBuf bytes.Buffer
	session.Stderr = &stderrBuf
	if err != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}

	go func() {
		w, err := session.StdinPipe()
		if err != nil {
			glog.Errorf("Unable to create stdin pipe", err)
			return
		}

		fmt.Fprintln(w, "C"+permissions, len(contents), filename)
		fmt.Fprintln(w, contents)
		fmt.Fprintln(w, "\x00")
		w.Close()
	}()

	// Scp returns non zero exit status even on normal cases,
	// therefore we need to verify the files are uploaded afterwards
	session.Run("scp -t " + directory)

	session, err = a.Client.NewSession()
	if err != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}

	err = session.Run("file " + remotePath)
	if err != nil {
		return errors.New("Failed to upload file: " + stderrBuf.String())
	}

	return nil
}

func NewScpClient(host string, config *ssh.ClientConfig) ScpClient {
	return ScpClient{
		Host:         host,
		ClientConfig: config,
	}
}
