package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/simpledb"
	"github.com/golang/glog"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
)

type KubernetesEndpoints struct {
	Uid            string
	DeploymentName string
	Endpoint       string
	Url            string
}

type KeyPair struct {
	DeploymentName string
	KeyName        string
	KeyFingerprint string
	KeyMaterial_1  string
	KeyMaterial_2  string
}

type AllowedPorts struct {
	Uid            string
	DeploymentName string
	Port           string
}

type ContainerEnvironment struct {
	Uid            string
	DeploymentName string
	TaskName       string
	ContainerName  string
	Value          string
	Name           string
}

type ContainerPortMappings struct {
	Uid            string
	DeploymentName string
	TaskName       string
	ContainerName  string
	HostPort       string
	ContainerPort  string
}

type ContainerMountPoints struct {
	Uid            string
	DeploymentName string
	TaskName       string
	ContainerName  string
	ReadOnly       string
	ContainerPath  string
	SourceVolume   string
}

type ContainerCommand struct {
	Uid            string
	DeploymentName string
	TaskName       string
	ContainerName  string
	CommandSeq     string
	CommandValue   string
}

type TaskVolumes struct {
	Uid            string
	DeploymentName string
	Name           string
	SourcePath     string
}

type TaskContainer struct {
	Uid            string
	DeploymentName string
	TaskName       string
	ContainerName  string
	ImageName      string
	Memory         string
	Cpu            string
}

type NodeMapping struct {
	Uid            string
	DeploymentName string
	NodeId         string
	InstanceArn    string
	TaskName       string
}

type NodeInfo struct {
	Uid            string
	DeploymentName string
	NodeId         string
	InstanceArn    string
	InstanceType   string
	PublicDnsName  string
	PrivateIp      string
}

type ClusterState struct {
	DeploymentName string
	Region         string
	DeployType     string // awsecs, kubernetes
	BastionIp      string
	MasterIp       string
	Status         string // deployed, fail, deleted
	CreateTime     string
}

type DB interface {
	Insert(deploymentStatus *DeploymentStatus) error
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

func createDomains(simpledbSvc *simpledb.SimpleDB) error {
	domainNames := []string{
		"ClusterState",
		"NodeInfo",
		"NodeMapping",
		"TaskContainer",
		"TaskVolumes",
		"ContainerCommand",
		"ContainerMountPoints",
		"ContainerPortMappings",
		"ContainerEnvironment",
		"AllowedPorts",
		"KeyPair",
	}

	for _, domainName := range domainNames {
		createDomainInput := &simpledb.CreateDomainInput{
			DomainName: aws.String(domainName),
		}

		if _, err := simpledbSvc.CreateDomain(createDomainInput); err != nil {
			return errors.New("Unable to create simpleDB domainName: " + domainName)
		}
	}

	return nil
}

func (db SimpleDB) Insert(deploymentStatus *DeploymentStatus) error {
	sess, err := createSession(db.Config, db.Region)
	if err != nil {
		return errors.New("Unable to create session: " + err.Error())
	}

	simpledbSvc := simpledb.New(sess)

	if err := createDomains(simpledbSvc); err != nil {
		return errors.New("Unable to create domains from simpleDB: " + err.Error())
	}

	// ClusterState
	if err := putClusterState(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put clusterState attributes to simpleDB: " + err.Error())
	}

	// Nodeinfo
	if err := putNodeInfo(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put nodeinfo attributes to simpleDB: " + err.Error())
	}

	// NodeMapping
	if err := putNodeMapping(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put nodeMapping attributes to simpleDB: " + err.Error())
	}

	// TaskContainer
	if err := putTaskContainer(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put taskContainer attributes to simpleDB: " + err.Error())
	}

	// TaskVolumes
	if err := putTaskVolumes(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put taskVolumes attributes to simpleDB: " + err.Error())
	}

	// ContainerCommand
	if err := putContainerCommand(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put containerCommand attributes to simpleDB: " + err.Error())
	}

	// ContainerMountPoints
	if err := putContainerMountPoints(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put containerMountPoints attributes to simpleDB: " + err.Error())
	}

	// ContainerPortMappings
	if err := putContainerPortMappings(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put containerPortMappings attributes to simpleDB: " + err.Error())
	}

	// ContainerEnvironment
	if err := putContainerEnvironment(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put containerEnvironment attributes to simpleDB: " + err.Error())
	}

	// AllowedPorts
	if err := putAllowedPorts(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put allowedPorts attributes to simpleDB: " + err.Error())
	}

	// KeyPair
	if err := putKeyPair(simpledbSvc, deploymentStatus); err != nil {
		return errors.New("Unable to put keyPair attributes to simpleDB: " + err.Error())
	}

	return nil
}

func (db SimpleDB) StoreDeploymentStatus(deploymentStatus *DeploymentStatus) error {
	domainName := "Hyperpilot"
	itemName := "deploymentStatus"

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
	domainName := "Hyperpilot"
	itemName := "deploymentStatus"

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

func putClusterState(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		deployType := "awsecs"
		masterIp := ""
		bastionIp := ""
		status := "deployed"

		if len(deploymentStatus.KubernetesClusters.Clusters) > 0 {
			deployType = "kubernetes"
			masterIp = deploymentStatus.KubernetesClusters.Clusters[deploymentName].MasterIp
			bastionIp = deploymentStatus.KubernetesClusters.Clusters[deploymentName].BastionIp
		}

		clusterState := &ClusterState{
			DeploymentName: deploymentName,
			Region:         deployedCluster.Deployment.Region,
			BastionIp:      bastionIp,
			MasterIp:       masterIp,
			DeployType:     deployType,
			Status:         status,
			CreateTime:     time.Now().Format("2006-01-02 15:04:05"),
		}

		putAttributesInput := &simpledb.PutAttributesInput{
			Attributes: appendAttributes(clusterState),
			DomainName: aws.String("ClusterState"),
			ItemName:   aws.String(clusterState.DeploymentName),
		}

		if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
			return errors.New("Unable to put attributes to simpleDB: " + err.Error())
		}
	}

	return nil
}

func putNodeInfo(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "NodeInfo"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for id, node := range deployedCluster.NodeInfos {
			nodeInfo := &NodeInfo{
				Uid:            uuid.NewUUID().String(),
				DeploymentName: deploymentName,
				NodeId:         strconv.Itoa(id),
				InstanceArn:    node.Arn,
				InstanceType:   aws.StringValue(node.Instance.InstanceType),
				PublicDnsName:  node.PublicDnsName,
				PrivateIp:      node.PrivateIp,
			}

			putAttributesInput := &simpledb.PutAttributesInput{
				Attributes: appendAttributes(nodeInfo),
				DomainName: aws.String(domainName),
				ItemName:   aws.String(nodeInfo.Uid),
			}

			if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
				return errors.New("Unable to put attributes to simpleDB: " + err.Error())
			}
		}
	}

	return nil
}

func putNodeMapping(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "NodeMapping"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	instanceArns := map[int]string{}
	for _, deployedCluster := range deploymentStatus.DeployedClusters {
		for id, node := range deployedCluster.NodeInfos {
			instanceArns[id] = node.Arn
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, node := range deployedCluster.Deployment.NodeMapping {
			nodeMapping := &NodeMapping{
				Uid:            uuid.NewUUID().String(),
				DeploymentName: deploymentName,
				NodeId:         strconv.Itoa(node.Id),
				InstanceArn:    instanceArns[node.Id],
				TaskName:       node.Task,
			}

			putAttributesInput := &simpledb.PutAttributesInput{
				Attributes: appendAttributes(nodeMapping),
				DomainName: aws.String(domainName),
				ItemName:   aws.String(nodeMapping.Uid),
			}

			if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
				return errors.New("Unable to put attributes to simpleDB: " + err.Error())
			}
		}
	}

	return nil
}

func putTaskContainer(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "TaskContainer"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, containerDefinition := range task.ContainerDefinitions {
				taskContainer := &TaskContainer{
					Uid:            uuid.NewUUID().String(),
					DeploymentName: deploymentName,
					TaskName:       aws.StringValue(task.Family),
					ContainerName:  aws.StringValue(containerDefinition.Name),
					ImageName:      aws.StringValue(containerDefinition.Image),
					Memory:         strconv.FormatInt(aws.Int64Value(containerDefinition.Memory), 10),
					Cpu:            strconv.FormatInt(aws.Int64Value(containerDefinition.Cpu), 10),
				}

				putAttributesInput := &simpledb.PutAttributesInput{
					Attributes: appendAttributes(taskContainer),
					DomainName: aws.String(domainName),
					ItemName:   aws.String(taskContainer.Uid),
				}

				if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
					return errors.New("Unable to put attributes to simpleDB: " + err.Error())
				}
			}
		}
	}

	return nil
}

func putTaskVolumes(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "TaskVolumes"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, volume := range task.Volumes {
				taskVolumes := &TaskVolumes{
					Uid:            uuid.NewUUID().String(),
					DeploymentName: deploymentName,
					Name:           aws.StringValue(volume.Name),
					SourcePath:     aws.StringValue(volume.Host.SourcePath),
				}

				putAttributesInput := &simpledb.PutAttributesInput{
					Attributes: appendAttributes(taskVolumes),
					DomainName: aws.String(domainName),
					ItemName:   aws.String(taskVolumes.Uid),
				}

				if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
					return errors.New("Unable to put attributes to simpleDB: " + err.Error())
				}
			}
		}
	}

	return nil
}

func putContainerCommand(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "ContainerCommand"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, containerDefinition := range task.ContainerDefinitions {
				for seq, command := range containerDefinition.Command {
					containerCommand := &ContainerCommand{
						Uid:            uuid.NewUUID().String(),
						DeploymentName: deploymentName,
						TaskName:       aws.StringValue(task.Family),
						ContainerName:  aws.StringValue(containerDefinition.Name),
						CommandSeq:     strconv.Itoa(seq),
						CommandValue:   aws.StringValue(command),
					}

					putAttributesInput := &simpledb.PutAttributesInput{
						Attributes: appendAttributes(containerCommand),
						DomainName: aws.String(domainName),
						ItemName:   aws.String(containerCommand.Uid),
					}

					if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
						return errors.New("Unable to put attributes to simpleDB: " + err.Error())
					}
				}
			}
		}
	}

	return nil
}

func putContainerMountPoints(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "ContainerMountPoints"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, containerDefinition := range task.ContainerDefinitions {
				for _, mountPoint := range containerDefinition.MountPoints {
					containerMountPoints := &ContainerMountPoints{
						Uid:            uuid.NewUUID().String(),
						DeploymentName: deploymentName,
						TaskName:       aws.StringValue(task.Family),
						ContainerName:  aws.StringValue(containerDefinition.Name),
						ReadOnly:       strconv.FormatBool(aws.BoolValue(mountPoint.ReadOnly)),
						ContainerPath:  aws.StringValue(mountPoint.ContainerPath),
						SourceVolume:   aws.StringValue(mountPoint.SourceVolume),
					}

					putAttributesInput := &simpledb.PutAttributesInput{
						Attributes: appendAttributes(containerMountPoints),
						DomainName: aws.String(domainName),
						ItemName:   aws.String(containerMountPoints.Uid),
					}

					if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
						return errors.New("Unable to put attributes to simpleDB: " + err.Error())
					}
				}
			}
		}
	}

	return nil
}

func putContainerPortMappings(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "ContainerPortMappings"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, containerDefinition := range task.ContainerDefinitions {
				for _, portMapping := range containerDefinition.PortMappings {
					containerPortMappings := &ContainerPortMappings{
						Uid:            uuid.NewUUID().String(),
						DeploymentName: deploymentName,
						TaskName:       aws.StringValue(task.Family),
						ContainerName:  aws.StringValue(containerDefinition.Name),
						HostPort:       strconv.FormatInt(aws.Int64Value(portMapping.HostPort), 10),
						ContainerPort:  strconv.FormatInt(aws.Int64Value(portMapping.ContainerPort), 10),
					}

					putAttributesInput := &simpledb.PutAttributesInput{
						Attributes: appendAttributes(containerPortMappings),
						DomainName: aws.String(domainName),
						ItemName:   aws.String(containerPortMappings.Uid),
					}

					if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
						return errors.New("Unable to put attributes to simpleDB: " + err.Error())
					}
				}
			}
		}
	}

	return nil
}

func putContainerEnvironment(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "ContainerEnvironment"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, task := range deployedCluster.Deployment.TaskDefinitions {
			for _, containerDefinition := range task.ContainerDefinitions {
				for _, env := range containerDefinition.Environment {
					containerEnvironment := &ContainerEnvironment{
						Uid:            uuid.NewUUID().String(),
						DeploymentName: deploymentName,
						TaskName:       aws.StringValue(task.Family),
						ContainerName:  aws.StringValue(containerDefinition.Name),
						Name:           aws.StringValue(env.Name),
						Value:          aws.StringValue(env.Value),
					}

					putAttributesInput := &simpledb.PutAttributesInput{
						Attributes: appendAttributes(containerEnvironment),
						DomainName: aws.String(domainName),
						ItemName:   aws.String(containerEnvironment.Uid),
					}

					if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
						return errors.New("Unable to put attributes to simpleDB: " + err.Error())
					}
				}
			}
		}
	}

	return nil
}

func putAllowedPorts(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "AllowedPorts"
	for deploymentName, _ := range deploymentStatus.DeployedClusters {
		if err := deleteRecordByDeploymentName(simpledbSvc, domainName, deploymentName); err != nil {
			return errors.New("Unable to delete attributes from simpleDB: " + err.Error())
		}
	}

	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		for _, allowedPort := range deployedCluster.Deployment.AllowedPorts {
			allowedPorts := &AllowedPorts{
				Uid:            uuid.NewUUID().String(),
				DeploymentName: deploymentName,
				Port:           strconv.Itoa(allowedPort),
			}

			putAttributesInput := &simpledb.PutAttributesInput{
				Attributes: appendAttributes(allowedPorts),
				DomainName: aws.String(domainName),
				ItemName:   aws.String(allowedPorts.Uid),
			}

			if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
				return errors.New("Unable to put attributes to simpleDB: " + err.Error())
			}
		}
	}

	return nil
}

func putKeyPair(simpledbSvc *simpledb.SimpleDB, deploymentStatus *DeploymentStatus) error {
	domainName := "KeyPair"
	for deploymentName, deployedCluster := range deploymentStatus.DeployedClusters {
		keyPair := &KeyPair{
			DeploymentName: deploymentName,
			KeyName:        aws.StringValue(deployedCluster.KeyPair.KeyName),
			KeyFingerprint: aws.StringValue(deployedCluster.KeyPair.KeyFingerprint),
			KeyMaterial_1:  aws.StringValue(deployedCluster.KeyPair.KeyMaterial)[:1024],
			KeyMaterial_2:  aws.StringValue(deployedCluster.KeyPair.KeyMaterial)[1024:],
		}

		putAttributesInput := &simpledb.PutAttributesInput{
			Attributes: appendAttributes(keyPair),
			DomainName: aws.String(domainName),
			ItemName:   aws.String(keyPair.DeploymentName),
		}

		if _, err := simpledbSvc.PutAttributes(putAttributesInput); err != nil {
			return errors.New("Unable to put attributes to simpleDB: " + err.Error())
		}
	}

	return nil
}

func appendAttributes(v interface{}) []*simpledb.ReplaceableAttribute {
	attributes := []*simpledb.ReplaceableAttribute{}
	s := reflect.ValueOf(v).Elem()
	for i := 0; i < s.NumField(); i++ {
		fieldName := s.Type().Field(i).Name
		fieldValue := fmt.Sprintf("%v", s.Field(i).Interface())

		if fieldName == "Uid" {
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

func deleteRecordByDeploymentName(simpledbSvc *simpledb.SimpleDB, domainName string, deploymentName string) error {
	selectExpression := fmt.Sprintf("select * from `%s` where DeploymentName='%s'", domainName, deploymentName)
	selectInput := &simpledb.SelectInput{
		SelectExpression: aws.String(selectExpression),
	}

	if selectOutput, err := simpledbSvc.Select(selectInput); err != nil {
		return errors.New("Unable to select data from simpleDB: " + err.Error())
	} else {
		if len(selectOutput.Items) == 0 {
			return nil
		}

		deletableItems := []*simpledb.DeletableItem{}
		for _, item := range selectOutput.Items {
			deletableItems = append(deletableItems, &simpledb.DeletableItem{
				Name: item.Name,
			})
		}

		batchDeleteAttributesInput := &simpledb.BatchDeleteAttributesInput{
			DomainName: aws.String(domainName),
			Items:      deletableItems,
		}

		if _, err := simpledbSvc.BatchDeleteAttributes(batchDeleteAttributesInput); err != nil {
			return errors.New("Unable to put attributes to simpleDB: " + err.Error())
		}
	}

	return nil
}
