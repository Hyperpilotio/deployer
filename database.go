package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/simpledb"
	"github.com/golang/glog"
	"github.com/spf13/viper"
)

var domainName = "hyperpilot"
var itemName = "deploymentStatus"

type DB interface {
	StoreDeploymentStatus(deploymentStatus *DeploymentStatus) error
	LoadDeploymentStatus() (*DeploymentStatus, error)
}

type SimpleDB struct {
	Config *viper.Viper
	Region string
}

func NewDB(config *viper.Viper) (DB, error) {
	dbType := strings.ToLower(config.GetString("database.type"))
	region := strings.ToLower(config.GetString("database.region"))

	switch dbType {
	case "simpledb":
		return SimpleDB{
			Config: config,
			Region: region,
		}, nil
	default:
		return nil, errors.New("Unsupported database type: " + dbType)
	}
}

func createSession(viper *viper.Viper, regionName string) (*session.Session, error) {
	awsId := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(regionName),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Unable to create session: %s", err)
		return nil, err
	}

	return sess, nil
}

func (db SimpleDB) createDomain(simpledbSvc *simpledb.SimpleDB, domainName string) error {
	createDomainInput := &simpledb.CreateDomainInput{
		DomainName: aws.String(domainName),
	}

	if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
		return errors.New("Unable to create simpleDB domainName: " + domainName)
	}

	return nil
}

func (db SimpleDB) StoreDeploymentStatus(deploymentStatus *DeploymentStatus) error {
	sess, err := createSession(db.Config, db.Region)
	if err != nil {
		return errors.New("Unable to create session: " + err.Error())
	}

	simpledbSvc := simpledb.New(sess)

	createDomainInput := &simpledb.CreateDomainInput{
		DomainName: aws.String(domainName),
	}

	if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
		return errors.New("Unable to create simpleSB domain: " + err.Error())
	}

	b, err := json.Marshal(deploymentStatus)
	if err != nil {
		return errors.New("Unable to marshal deploymentStatus: " + err.Error())
	}

	jsonString := string(b)
	splitLen := 1024
	valLen := 0
	if len(jsonString)%splitLen == 0 {
		valLen = len(jsonString) / splitLen
	} else {
		valLen = len(jsonString)/splitLen + 1
	}

	attributes := []*simpledb.ReplaceableAttribute{}
	for i := 0; i < valLen; i++ {
		lastLndex := (i + 1) * splitLen
		if lastLndex > len(jsonString) {
			lastLndex = len(jsonString)
		}

		attribute := &simpledb.ReplaceableAttribute{
			Name:    aws.String(strconv.Itoa(i + 1)),
			Value:   aws.String(jsonString[i*splitLen : lastLndex]),
			Replace: aws.Bool(true),
		}

		attributes = append(attributes, attribute)
	}

	deleteAttributesInput := &simpledb.DeleteAttributesInput{
		DomainName: aws.String(domainName),
		ItemName:   aws.String(itemName),
	}

	if _, err := simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return errors.New("Unable to delete attributes: " + err.Error())
	}

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: attributes,
		DomainName: aws.String(domainName),
		ItemName:   aws.String(itemName),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db SimpleDB) LoadDeploymentStatus() (*DeploymentStatus, error) {
	sess, err := createSession(db.Config, db.Region)
	if err != nil {
		return nil, errors.New("Unable to create session: " + err.Error())
	}

	simpledbSvc := simpledb.New(sess)

	getAttributesInput := &simpledb.GetAttributesInput{
		DomainName: aws.String(domainName),
		ItemName:   aws.String(itemName),
	}

	deploymentStatus := &DeploymentStatus{}
	if resp, err := simpledbSvc.GetAttributes(getAttributesInput); err != nil {
		return nil, errors.New("Unable to get attributes from simpleDB: " + err.Error())
	} else {
		attrCnt := len(resp.Attributes)
		jsonStr := ""
		for i := 1; i <= attrCnt; i++ {
			for _, attribute := range resp.Attributes {
				if strconv.Itoa(i) == aws.StringValue(attribute.Name) {
					jsonStr = jsonStr + aws.StringValue(attribute.Value)
					break
				}
			}
		}

		if jsonStr != "" {
			if err := json.Unmarshal([]byte(jsonStr), deploymentStatus); err != nil {
				return nil, errors.New("Unable to unmarshal json to deploymentStatus: " + err.Error())
			}
		}
	}

	return deploymentStatus, nil
}
