package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/simpledb"
	"github.com/golang/glog"
	"github.com/spf13/viper"
)

type ClusterState struct {
	DeploymentName    string
	Region            string
	BastionIp         string
	MasterIp          string
	KubeConfigCommand string
	KubeConfigPath    string
	KeyName           string
	KeyFingerprint    string
	KeyMaterial_1     string
	KeyMaterial_2     string
	SecurityGroupId   string
	SubnetId          string
	InternetGatewayId string
	VpcId             string
	InstanceIds       string
}

type DB interface {
	StoreClusterState(clusterState *ClusterState) error
	LoadClusterState() ([]*ClusterState, error)
	DeleteClusterState(itemName *string) error
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

func (db SimpleDB) StoreClusterState(clusterState *ClusterState) error {
	domainName := "ClusterState"
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

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: appendAttributes(clusterState),
		DomainName: aws.String(domainName),
		ItemName:   aws.String(clusterState.DeploymentName),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db SimpleDB) LoadClusterState() ([]*ClusterState, error) {
	domainName := "ClusterState"

	sess, err := createSession(db.Config, db.Region)
	if err != nil {
		return nil, errors.New("Unable to create session: " + err.Error())
	}

	simpledbSvc := simpledb.New(sess)

	selectExpression := fmt.Sprintf("select * from `%s`", domainName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	clusterStates := []*ClusterState{}
	if selectOutput, err := simpledbSvc.Select(selectInput); err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	} else {
		if len(selectOutput.Items) == 0 {
			return nil, errors.New("Unable to find data from " + domainName)
		}

		for _, item := range selectOutput.Items {
			getAttributesInput := &simpledb.GetAttributesInput{
				DomainName: aws.String(domainName),
				ItemName:   item.Name,
			}

			if resp, err := simpledbSvc.GetAttributes(getAttributesInput); err != nil {
				return nil, errors.New("Unable to put attributes to simpleDB: " + err.Error())
			} else {
				clusterState := &ClusterState{
					DeploymentName: aws.StringValue(item.Name),
				}

				for _, attribute := range resp.Attributes {
					attrName := aws.StringValue(attribute.Name)
					attrvalue := aws.StringValue(attribute.Value)

					v := reflect.ValueOf(clusterState).Elem()
					t := v.Type()
					for i := 0; i < t.NumField(); i++ {
						fieldVal := v.Field(i)
						if attrName == t.Field(i).Name {
							fieldVal.Set(reflect.ValueOf(attrvalue))
						}
					}
				}

				clusterStates = append(clusterStates, clusterState)
			}
		}
	}

	return clusterStates, nil
}

func (db SimpleDB) DeleteClusterState(itemName *string) error {
	domainName := "ClusterState"

	sess, err := createSession(db.Config, db.Region)
	if err != nil {
		return errors.New("Unable to create session: " + err.Error())
	}

	simpledbSvc := simpledb.New(sess)

	deleteAttributesInput := &simpledb.DeleteAttributesInput{
		DomainName: aws.String(domainName),
		ItemName:   itemName,
	}

	if _, err := simpledbSvc.DeleteAttributes(deleteAttributesInput); err != nil {
		return errors.New("Unable to delete clusterState from simpleDB: " + err.Error())
	}

	return nil
}

func appendAttributes(v interface{}) []*simpledb.ReplaceableAttribute {
	attributes := []*simpledb.ReplaceableAttribute{}
	s := reflect.ValueOf(v).Elem()
	for i := 0; i < s.NumField(); i++ {
		fieldName := s.Type().Field(i).Name
		fieldValue := fmt.Sprintf("%v", s.Field(i).Interface())

		if fieldName == "DeploymentName" {
			continue
		}

		attributes = append(attributes, &simpledb.ReplaceableAttribute{
			Name:    aws.String(fieldName),
			Value:   aws.String(fieldValue),
			Replace: aws.Bool(true),
		})
	}

	return attributes
}
