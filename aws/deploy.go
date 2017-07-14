package aws

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"github.com/golang/glog"
	"golang.org/x/crypto/ssh"
)

type AWSProfile struct {
	UserId    string
	AwsId     string
	AwsSecret string
}

type NodeInfo struct {
	Instance      *ec2.Instance
	Arn           string
	PublicDnsName string
	PrivateIp     string
}

// AWSCluster stores the data of a aws backed cluster
type AWSCluster struct {
	Region            string
	AWSProfile        *AWSProfile
	Name              string
	KeyPair           *ec2.CreateKeyPairOutput
	SecurityGroupId   string
	SubnetId          string
	InternetGatewayId string
	NodeInfos         map[int]*NodeInfo
	InstanceIds       []*string
	VpcId             string
}

func CreateSession(awsProfile *AWSProfile, region string) (*session.Session, error) {
	awsId := awsProfile.AwsId
	awsSecret := awsProfile.AwsSecret
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(region),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Unable to create session: %s", err)
		return nil, err
	}

	return sess, nil
}

func CreateKeypair(ec2Svc *ec2.EC2, name string) (*ec2.CreateKeyPairOutput, error) {
	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(name),
	}

	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		return nil, errors.New("Unable to create key pair: " + keyErr.Error())
	}

	return keyOutput, nil
}

func NewAWSCluster(name string, region string, awsProfile *AWSProfile) *AWSCluster {
	return &AWSCluster{
		AWSProfile:  awsProfile,
		Name:        name,
		Region:      region,
		NodeInfos:   make(map[int]*NodeInfo),
		InstanceIds: make([]*string, 0),
	}
}

// KeyName return a key name according to the Deployment.Name with suffix "-key"
func (awsCluster *AWSCluster) KeyName() string {
	return awsCluster.Name + "-key"
}

func (awsCluster *AWSCluster) StackName() string {
	return awsCluster.Name + "-stack"
}

// PolicyName return a key name according to the Deployment.Name with suffix "-policy"
func (awsCluster *AWSCluster) PolicyName() string {
	return awsCluster.Name + "-policy"
}

// RoleName return a key name according to the Name with suffix "-role"
func (awsCluster *AWSCluster) RoleName() string {
	return awsCluster.Name + "-role"
}

// VPCName return a key name according to the Name with suffix "-vpc"
func (awsCluster AWSCluster) VPCName() string {
	return awsCluster.Name + "-vpc"
}

// SubnetName return a key name according to the Name with suffix "-vpc"
func (awsCluster AWSCluster) SubnetName() string {
	return awsCluster.Name + "-subnet"
}

func (awsCluster AWSCluster) SshConfig(user string) (*ssh.ClientConfig, error) {
	privateKey := strings.Replace(*awsCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}

	return clientConfig, nil
}

// ReloadKeyPair reload KeyPair by keyName
func (awsCluster *AWSCluster) ReloadKeyPair(keyMaterial string) error {
	awsProfile := awsCluster.AWSProfile

	sess, sessionErr := CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	describeKeyPairsInput := &ec2.DescribeKeyPairsInput{
		KeyNames: []*string{
			aws.String(awsCluster.KeyName()),
		},
	}

	describeKeyPairsOutput, err := ec2Svc.DescribeKeyPairs(describeKeyPairsInput)
	if err != nil {
		return fmt.Errorf("Unable to describe keyPairs: %s", err.Error())
	}

	if len(describeKeyPairsOutput.KeyPairs) == 0 {
		return fmt.Errorf("Unable to find %s keyPairs", awsCluster.Name)
	}

	keyPair := &ec2.CreateKeyPairOutput{
		KeyName:        describeKeyPairsOutput.KeyPairs[0].KeyName,
		KeyFingerprint: describeKeyPairsOutput.KeyPairs[0].KeyFingerprint,
		KeyMaterial:    aws.String(keyMaterial),
	}
	awsCluster.KeyPair = keyPair

	return nil
}
