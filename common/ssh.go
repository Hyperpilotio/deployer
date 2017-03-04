package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/sftp"

	"golang.org/x/crypto/ssh"
)

type SshClient struct {
	Host         string
	BastionHost  string
	ClientConfig *ssh.ClientConfig
}

func (a *SshClient) connectViaBastion() (*ssh.Client, *ssh.Client, error) {
	bastion, err := ssh.Dial("tcp", a.BastionHost, a.ClientConfig)
	if err != nil {
		return nil, nil, errors.New("Unable to connect to bastion: " + err.Error())
	}

	conn, err := bastion.Dial("tcp", a.Host)
	if err != nil {
		bastion.Close()
		return nil, nil, errors.New("Unable to connect to server from bastion: " + err.Error())
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, a.Host, a.ClientConfig)
	if err != nil {
		conn.Close()
		bastion.Close()
		return nil, nil, errors.New("Unable to create new client conn: " + err.Error())
	}

	client := ssh.NewClient(ncc, chans, reqs)

	return client, bastion, nil
}

// Connects to the remote SSH server, returns error if it couldn't establish a session to the SSH server
func (a *SshClient) connect() (*ssh.Client, *ssh.Client, error) {
	maxRetries := 5
	var sshError error
	for i := 1; i <= maxRetries; i++ {
		if a.BastionHost != "" {
			client, bastion, err := a.connectViaBastion()
			if err != nil {
				sshError = err
			} else {
				return client, bastion, nil
			}
		} else {
			client, err := ssh.Dial("tcp", a.Host, a.ClientConfig)
			if err != nil {
				sshError = errors.New("Unable to connect to server: " + err.Error())
				glog.Infof("Unable to ssh to %s, retrying %d time", a.Host, i)
			} else {
				return client, nil, nil
			}
		}

		time.Sleep(time.Duration(10) * time.Second)
	}

	return nil, nil, sshError
}

func (a *SshClient) RunCommand(command string, verbose bool) error {
	client, bastion, connectErr := a.connect()
	if connectErr != nil {
		return connectErr
	}

	if bastion != nil {
		defer bastion.Close()
	}
	defer client.Close()

	session, err := client.NewSession()
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
	client, bastion, connectErr := a.connect()
	if connectErr != nil {
		return connectErr
	}

	if bastion != nil {
		defer bastion.Close()
	}
	defer client.Close()

	contents_bytes, _ := ioutil.ReadAll(fileReader)
	contents := string(contents_bytes)
	filename := path.Base(remotePath)
	directory := path.Dir(remotePath)

	session, err := client.NewSession()
	if err != nil {
		return errors.New("Unable to create ssh session: " + err.Error())
	}
	session.Run("mkdir -p " + directory)

	session, err = client.NewSession()
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

	newSession, newErr := client.NewSession()
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

func (a *SshClient) CopyFileToLocal(remotePath string, localPath string) error {
	client, bastion, connectErr := a.connect()
	if connectErr != nil {
		return connectErr
	}

	if bastion != nil {
		defer bastion.Close()
	}
	defer client.Close()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return errors.New("Unable to create sftp client: " + err.Error())
	}

	srcFile, err := sftpClient.Open(remotePath)
	if err != nil {
		return errors.New("Unable to open remote path: " + err.Error())
	}
	defer srcFile.Close()

	dstFile, err := os.Create(localPath)
	if err != nil {
		return errors.New("Unable to open local path:" + err.Error())
	}
	defer dstFile.Close()

	srcFile.WriteTo(dstFile)

	return nil
}

func NewSshClient(host string, config *ssh.ClientConfig, bastionHost string) SshClient {
	return SshClient{
		Host:         host,
		BastionHost:  bastionHost,
		ClientConfig: config,
	}
}
