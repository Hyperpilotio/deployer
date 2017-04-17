package store

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

type SimpleDB struct {
	Region string
	Domain string
	Sess   *session.Session
}

func NewSimpleDB(config *viper.Viper) (*SimpleDB, error) {
	region := strings.ToLower(config.GetString("store.region"))
	domain := strings.ToLower(config.GetString("store.domain"))
	if session, err := createSessionByRegion(config, region); err != nil {
		return nil, err
	} else {
		return &SimpleDB{
			Region: region,
			Domain: domain,
			Sess:   session,
		}, nil
	}
}

func (db *SimpleDB) StoreNewDeployment(deployment *Deployment) error {
	simpledbSvc := simpledb.New(db.Sess)

	createDomainInput := &simpledb.CreateDomainInput{
		DomainName: aws.String(db.Domain),
	}

	if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
		return errors.New("Unable to create simpleSB domain: " + err.Error())
	}

	putAttributesInput := &simpledb.PutAttributesInput{
		Attributes: appendAttributes(deployment),
		DomainName: aws.String(db.Domain),
		ItemName:   aws.String(deployment.Name),
	}

	if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
		return errors.New("Unable to put attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db *SimpleDB) LoadDeployment() ([]*Deployment, error) {
	simpledbSvc := simpledb.New(db.Sess)

	selectExpression := fmt.Sprintf("select * from `%s`", db.Domain)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	deployments := []*Deployment{}
	if selectOutput, err := simpledbSvc.Select(selectInput); err != nil {
		return nil, errors.New("Unable to select data from simpleDB: " + err.Error())
	} else {
		if len(selectOutput.Items) == 0 {
			return nil, errors.New("Unable to find data from " + db.Domain)
		}

		for _, item := range selectOutput.Items {
			getAttributesInput := &simpledb.GetAttributesInput{
				DomainName: aws.String(db.Domain),
				ItemName:   item.Name,
			}

			if resp, err := simpledbSvc.GetAttributes(getAttributesInput); err != nil {
				return nil, errors.New("Unable to put attributes to simpleDB: " + err.Error())
			} else {
				deployment := &Deployment{
					Name: aws.StringValue(item.Name),
				}

				for _, attribute := range resp.Attributes {
					attrName := aws.StringValue(attribute.Name)
					attrvalue := aws.StringValue(attribute.Value)

					v := reflect.ValueOf(deployment).Elem()
					t := v.Type()
					for i := 0; i < t.NumField(); i++ {
						fieldVal := v.Field(i)
						if attrName == t.Field(i).Name {
							fieldVal.Set(reflect.ValueOf(attrvalue))
						}
					}
				}

				deployments = append(deployments, deployment)
			}
		}
	}

	return deployments, nil
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

func createSessionByRegion(viper *viper.Viper, regionName string) (*session.Session, error) {
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
