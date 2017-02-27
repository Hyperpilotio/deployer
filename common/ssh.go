package common

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"os"
	"path"

	"github.com/golang/glog"
)

type SshClient struct {
	Host         string
	ClientConfig *ssh.ClientConfig
	Client       *ssh.Client
}

// Connects to the remote SSH server, returns error if it couldn't establish a session to the SSH server
func (a *SshClient) Connect() error {
	client, err := ssh.Dial("tcp", a.Host, a.ClientConfig)
	if err != nil {
		return err
	}

	a.Client = client

	return nil
}

func (a *SshClient) RunCommand(command string, verbose bool) error {
	session, err := a.Client.NewSession()
	if err != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}
	defer session.Close()

	var stderrBuf bytes.Buffer
	if verbose {
		session.Stderr = os.Stderr
		session.Stdout = os.Stdout
	} else {
		session.Stderr = &stderrBuf
	}

	err = session.Run(command)
	if err != nil {
		if verbose {
			return err
		} else {
			return errors.New("Unable to run command: " + stderrBuf.String())
		}
	}

	return nil
}

// Copies the contents of an io.Reader to a remote location
func (a *SshClient) CopyFile(fileReader io.Reader, remotePath string, permissions string) error {
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
		session.Close()
	}()

	// Scp returns non zero exit status even on normal cases,
	// therefore we need to verify the files are uploaded afterwards
	session.Run("scp -t " + directory)

	newSession, newErr := a.Client.NewSession()
	if newErr != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}
	defer newSession.Close()

	err = newSession.Run("file " + remotePath)
	if err != nil {
		return errors.New("Failed to upload file: " + stderrBuf.String())
	}

	return nil
}

func NewSshClient(host string, config *ssh.ClientConfig) SshClient {
	return SshClient{
		Host:         host,
		ClientConfig: config,
	}
}
