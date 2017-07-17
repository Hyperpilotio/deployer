package kubernetes

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	"github.com/hyperpilotio/deployer/common"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/funcs"
	"github.com/hyperpilotio/go-utils/log"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var publicPortType = 1

// NewDeployer return the K8S of Deployer
func NewDeployer(config *viper.Viper, awsProfile *hpaws.AWSProfile, deployment *apis.Deployment) (*K8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	awsCluster := hpaws.NewAWSCluster(deployment.Name, deployment.Region, awsProfile)
	deployer := &K8SDeployer{
		Config:        config,
		AWSCluster:    awsCluster,
		Deployment:    deployment,
		DeploymentLog: log,
		Services:      make(map[string]ServiceMapping),
	}

	return deployer, nil
}

func (k8sDeployer *K8SDeployer) GetLog() *log.FileLog {
	return k8sDeployer.DeploymentLog
}

func (k8sDeployer *K8SDeployer) GetScheduler() *job.Scheduler {
	return k8sDeployer.Scheduler
}

func (k8sDeployer *K8SDeployer) SetScheduler(sheduler *job.Scheduler) {
	k8sDeployer.Scheduler = sheduler
}

func (k8sDeployer *K8SDeployer) GetKubeConfigPath() string {
	return k8sDeployer.KubeConfigPath
}

// CreateDeployment start a deployment
func (k8sDeployer *K8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	if err := deployCluster(k8sDeployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	response := &CreateDeploymentResponse{
		Name:      k8sDeployer.Deployment.Name,
		Services:  k8sDeployer.Services,
		BastionIp: k8sDeployer.BastionIp,
		MasterIp:  k8sDeployer.MasterIp,
	}

	return response, nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (k8sDeployer *K8SDeployer) UpdateDeployment(deployment *apis.Deployment) error {
	k8sDeployer.Deployment = deployment
	awsCluster := k8sDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	stackName := awsCluster.StackName()
	log := k8sDeployer.DeploymentLog.Logger

	log.Info("Updating kubernetes deployment")
	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := deleteK8S(getAllDeployedNamespaces(deployment), k8sDeployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return errors.New("Unable to create aws session for delete: " + sessionErr.Error())
	}

	elbSvc := elb.New(sess)
	ec2Svc := ec2.New(sess)

	if err := deleteLoadBalancers(elbSvc, true, stackName, log); err != nil {
		log.Warningf("Unable to delete load balancers: " + err.Error())
	}

	if err := deleteElbSecurityGroup(ec2Svc, stackName, log); err != nil {
		log.Warningf("Unable to deleting elb securityGroups: %s", err.Error())
	}

	if err := k8sDeployer.deployKubernetesObjects(k8sClient, true); err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}

	return nil
}

func (k8sDeployer *K8SDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes: " + err.Error())
	}

	originalDeployment := k8sDeployer.Deployment
	k8sDeployer.Deployment = extensions
	if err := k8sDeployer.deployKubernetesObjects(k8sClient, true); err != nil {
		k8sDeployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}

	k8sDeployer.Deployment = newDeployment
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (k8sDeployer *K8SDeployer) DeleteDeployment() error {
	deleteDeployment(k8sDeployer)
	return nil
}

func populateNodeInfos(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("k8s-node"),
				},
			},
			{
				Name: aws.String("tag:KubernetesCluster"),
				Values: []*string{
					aws.String(awsCluster.StackName()),
				},
			},
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
				},
			},
		},
	}
	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	i := 1
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			nodeInfo := &hpaws.NodeInfo{
				Instance:  instance,
				PrivateIp: *instance.PrivateIpAddress,
			}
			awsCluster.NodeInfos[i] = nodeInfo
			i += 1
		}
	}

	return nil
}

func uploadFiles(awsCluster *hpaws.AWSCluster,
	deployment *apis.Deployment,
	uploadedFiles map[string]string,
	bastionIp string, log *logging.Logger) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	clientConfig, clientConfigErr := awsCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	for _, nodeInfo := range awsCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PrivateIp+":22", clientConfig, bastionIp+":22")
		// TODO: Refactor this so can be reused with AWS
		for _, deployFile := range deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployment.UserId+"_"+deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := sshClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				return fmt.Errorf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, nodeInfo.PrivateIp, err.Error())
			}
		}
	}

	log.Info("Uploaded all files")
	return nil
}

func getExistingNamespaces(k8sClient *k8s.Clientset) (map[string]bool, error) {
	namespaces := map[string]bool{}
	k8sNamespaces := k8sClient.CoreV1().Namespaces()
	existingNamespaces, err := k8sNamespaces.List(metav1.ListOptions{})
	if err != nil {
		return namespaces, fmt.Errorf("Unable to get existing namespaces: " + err.Error())
	}

	for _, existingNamespace := range existingNamespaces.Items {
		namespaces[existingNamespace.Name] = true
	}

	return namespaces, nil
}

func deployCluster(k8sDeployer *K8SDeployer, uploadedFiles map[string]string) error {
	deployment := k8sDeployer.Deployment
	awsCluster := k8sDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	log := k8sDeployer.DeploymentLog.Logger

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	if keyOutput, err := hpaws.CreateKeypair(ec2Svc, awsCluster.KeyName()); err != nil {
		return errors.New("Unable to create key pair: " + err.Error())
	} else {
		awsCluster.KeyPair = keyOutput
	}

	if err := deployKubernetes(sess, k8sDeployer); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := populateNodeInfos(ec2Svc, awsCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := uploadFiles(awsCluster, deployment, uploadedFiles, k8sDeployer.BastionIp, log); err != nil {
		deleteDeploymentOnFailure(k8sDeployer)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, awsCluster, deployment, log); err != nil {
		deleteDeploymentOnFailure(k8sDeployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sDeployer.deployKubernetesObjects(k8sClient, false); err != nil {
		deleteDeploymentOnFailure(k8sDeployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	return nil
}

func (k8sDeployer *K8SDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset, skipDelete bool) error {
	namespaces, namespacesErr := getExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		if !skipDelete {
			deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := createSecrets(k8sClient, namespaces, k8sDeployer.Deployment); err != nil {
		if !skipDelete {
			deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := k8sDeployer.deployServices(k8sClient, namespaces); err != nil {
		if !skipDelete {
			deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	k8sDeployer.recordPublicEndpoints(k8sClient)

	return nil
}

func deployKubernetes(sess *session.Session, k8sDeployer *K8SDeployer) error {
	awsCluster := k8sDeployer.AWSCluster
	deployment := k8sDeployer.Deployment
	log := k8sDeployer.DeploymentLog.Logger

	cfSvc := cloudformation.New(sess)
	params := &cloudformation.CreateStackInput{
		StackName: aws.String(awsCluster.StackName()),
		Capabilities: []*string{
			aws.String("CAPABILITY_NAMED_IAM"),
		},
		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("AdminIngressLocation"),
				ParameterValue: aws.String("0.0.0.0/0"),
			},
			{
				ParameterKey:   aws.String("KeyName"),
				ParameterValue: aws.String(awsCluster.KeyName()),
			},
			{
				ParameterKey:   aws.String("NetworkingProvider"),
				ParameterValue: aws.String("weave"),
			},
			{
				ParameterKey: aws.String("K8sNodeCapacity"),
				ParameterValue: aws.String(
					strconv.Itoa(len(deployment.ClusterDefinition.Nodes))),
			},
			{
				ParameterKey: aws.String("InstanceType"),
				ParameterValue: aws.String(
					deployment.ClusterDefinition.Nodes[0].InstanceType),
			},
			{
				ParameterKey:   aws.String("DiskSizeGb"),
				ParameterValue: aws.String("40"),
			},
			{
				ParameterKey:   aws.String("BastionInstanceType"),
				ParameterValue: aws.String("t2.micro"),
			},
			{
				ParameterKey:   aws.String("QSS3BucketName"),
				ParameterValue: aws.String("heptio-aws-quickstart-test"),
			},
			{
				ParameterKey:   aws.String("QSS3KeyPrefix"),
				ParameterValue: aws.String("heptio/kubernetes/master"),
			},
			{
				ParameterKey:   aws.String("AvailabilityZone"),
				ParameterValue: aws.String(awsCluster.Region + "b"),
			},
		},
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String("deployment"),
				Value: aws.String(awsCluster.Name),
			},
		},
		TemplateURL:      aws.String("https://hyperpilot-snap-collectors.s3.amazonaws.com/kubernetes-cluster-with-new-vpc-1.6.2.template"),
		TimeoutInMinutes: aws.Int64(30),
	}
	log.Info("Creating kubernetes stack...")
	if _, err := cfSvc.CreateStack(params); err != nil {
		return errors.New("Unable to create stack: " + err.Error())
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(awsCluster.StackName()),
	}

	log.Info("Waiting until stack is completed...")
	if err := cfSvc.WaitUntilStackCreateComplete(describeStacksInput); err != nil {
		return errors.New("Unable to wait until stack complete: " + err.Error())
	}

	log.Info("Kuberenete stack completed")
	describeStacksOutput, describeStacksErr := cfSvc.DescribeStacks(describeStacksInput)
	if describeStacksErr != nil {
		return errors.New("Unable to get stack outputs: " + describeStacksErr.Error())
	}

	outputs := describeStacksOutput.Stacks[0].Outputs
	sshProxyCommand := ""
	getKubeConfigCommand := ""
	for _, output := range outputs {
		if *output.OutputKey == "SSHProxyCommand" {
			sshProxyCommand = *output.OutputValue
		} else if *output.OutputKey == "GetKubeConfigCommand" {
			getKubeConfigCommand = *output.OutputValue
		}
	}

	if sshProxyCommand == "" {
		return errors.New("Unable to find SSHProxyCommand in stack output")
	} else if getKubeConfigCommand == "" {
		return errors.New("Unable to find GetKubeConfigCommand in stack output")
	}

	// We expect the SSHProxyCommand to be in the following format:
	//"ssh -A -L8080:localhost:8080 -o ProxyCommand='ssh ubuntu@111.111.111.111 nc 10.0.0.0 22' ubuntu@10.0.0.0"
	addresses := []string{}
	for _, part := range strings.Split(sshProxyCommand, " ") {
		if strings.HasPrefix(part, "ubuntu@") {
			addresses = append(addresses, strings.Split(part, "@")[1])
		}
	}

	if len(addresses) != 2 {
		return errors.New("Unexpected ssh proxy command format: " + sshProxyCommand)
	}

	k8sDeployer.BastionIp = addresses[0]
	k8sDeployer.MasterIp = addresses[1]

	if err := k8sDeployer.DownloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}

	log.Infof("Downloaded kube config at %s", k8sDeployer.KubeConfigPath)

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployer.KubeConfigPath); err != nil {
		return errors.New("Unable to parse kube config: " + err.Error())
	} else {
		k8sDeployer.KubeConfig = kubeConfig
	}

	if err := k8sDeployer.UploadSshKeyToBastion(); err != nil {
		return errors.New("Unable to upload sshKey: " + err.Error())
	}

	return nil
}

func deleteSecurityGroup(ec2Svc *ec2.EC2, vpcID *string, log *logging.Logger) error {
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcID},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	for _, group := range resp.SecurityGroups {
		if aws.StringValue(group.GroupName) == "default" {
			continue
		}
		params := &ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func waitUntilInternetGatewayDeleted(ec2Svc *ec2.EC2, deploymentName string,
	timeout time.Duration, log *logging.Logger) error {
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				params := &ec2.DescribeTagsInput{
					Filters: []*ec2.Filter{
						{
							Name: aws.String("resource-type"),
							Values: []*string{
								aws.String("internet-gateway"),
							},
						},
						{
							Name: aws.String("tag:deployment"),
							Values: []*string{
								aws.String(deploymentName),
							},
						},
					},
				}

				resp, _ := ec2Svc.DescribeTags(params)
				if resp.Tags == nil {
					c <- true
				}
				time.Sleep(time.Second * 10)
			}
		}
	}()

	select {
	case <-c:
		log.Info("InternetGateway is deleted")
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for InternetGateway to be deleted")
	}
}

func waitUntilNetworkInterfaceIsAvailable(ec2Svc *ec2.EC2, filters []*ec2.Filter,
	timeout time.Duration, log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		params := &ec2.DescribeNetworkInterfacesInput{
			Filters: filters,
		}

		allAvailable := true
		resp, _ := ec2Svc.DescribeNetworkInterfaces(params)
		for _, nif := range resp.NetworkInterfaces {
			if aws.StringValue(nif.Status) != "available" {
				allAvailable = false
			}
		}
		if allAvailable || len(resp.NetworkInterfaces) == 0 {
			log.Info("NetworkInterfaces are available")
			return true, nil
		}
		return false, nil
	})
}

func deleteNetworkInterfaces(ec2Svc *ec2.EC2, filters []*ec2.Filter, log *logging.Logger) error {
	errBool := false
	describeNetworkInterfacesInput := &ec2.DescribeNetworkInterfacesInput{
		Filters: filters,
	}

	resp, err := ec2Svc.DescribeNetworkInterfaces(describeNetworkInterfacesInput)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of network interface: %s\n", err.Error())
	}

	for _, nif := range resp.NetworkInterfaces {
		if nif.Attachment != nil && nif.Attachment.AttachmentId != nil {
			detachNetworkInterfaceInput := &ec2.DetachNetworkInterfaceInput{
				AttachmentId: nif.Attachment.AttachmentId,
				Force:        aws.Bool(true),
			}
			if _, err := ec2Svc.DetachNetworkInterface(detachNetworkInterfaceInput); err != nil {
				return fmt.Errorf("Unable to detach network interface: %s\n", err.Error())
			}
		}
	}

	if err := waitUntilNetworkInterfaceIsAvailable(ec2Svc, filters, time.Duration(60)*time.Second, log); err != nil {
		return fmt.Errorf("Unable to wait until network interface is available: %s\n", err.Error())
	}

	for _, nif := range resp.NetworkInterfaces {
		deleteNetworkInterfaceInput := &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: nif.NetworkInterfaceId,
		}
		if _, err := ec2Svc.DeleteNetworkInterface(deleteNetworkInterfaceInput); err != nil {
			log.Warningf("Unable to delete network interface: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative network interface")
	}

	return nil
}

func deleteClusterSecurityGroupNetworkInterfaces(ec2Svc *ec2.EC2,
	clusterSecurityGroupId string, log *logging.Logger) error {
	// If in-cluster ec2 Instance terminate, network interfaces need to delete
	log.Infof("Deleting clusterSecurityGroup network interfaces...")
	filters := []*ec2.Filter{&ec2.Filter{
		Name:   aws.String("group-id"),
		Values: []*string{aws.String(clusterSecurityGroupId)},
	}}

	if err := deleteNetworkInterfaces(ec2Svc, filters, log); err != nil {
		log.Warningf("Unable to delete network interfaces: %s", err.Error())
	}

	return nil
}

func deleteElbSecurityGroup(ec2Svc *ec2.EC2, stackName string, log *logging.Logger) error {
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:KubernetesCluster"),
				Values: []*string{aws.String(stackName)},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	k8sClusterSgId := ""
	k8sElbSgIds := []*string{}
	k8sElbSgNames := []*string{}
	for _, securityGroup := range resp.SecurityGroups {
		if strings.HasPrefix(aws.StringValue(securityGroup.GroupName), "k8s-elb") {
			k8sElbSgNames = append(k8sElbSgNames, securityGroup.GroupName)
			k8sElbSgIds = append(k8sElbSgIds, securityGroup.GroupId)
		}

		for _, tag := range securityGroup.Tags {
			tagKey := aws.StringValue(tag.Key)
			tagVal := aws.StringValue(tag.Value)
			if tagKey == "Name" && tagVal == "k8s-cluster-security-group" {
				k8sClusterSgId = aws.StringValue(securityGroup.GroupId)
				break
			}
		}
	}

	filters := []*ec2.Filter{&ec2.Filter{
		Name:   aws.String("group-name"),
		Values: k8sElbSgNames,
	}}

	if err := deleteNetworkInterfaces(ec2Svc, filters, log); err != nil {
		log.Warningf("Unable to delete network interfaces: %s", err.Error())
	}

	for _, sgId := range k8sElbSgIds {
		revokeSecurityGroupIngressInput := &ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(k8sClusterSgId),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol: aws.String("-1"),
					UserIdGroupPairs: []*ec2.UserIdGroupPair{
						{
							GroupId: sgId,
						},
					},
				},
			},
		}

		if _, err := ec2Svc.RevokeSecurityGroupIngress(revokeSecurityGroupIngressInput); err != nil {
			log.Warningf("Unable to remove ingress rules from security group: %s", err.Error())
		}

		params := &ec2.DeleteSecurityGroupInput{
			GroupId: sgId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func deleteLoadBalancers(elbSvc *elb.ELB, skipApiServer bool, stackName string, log *logging.Logger) error {
	log.Infof("Deleting loadBalancer...")
	deploymentLoadBalancers := &DeploymentLoadBalancers{
		StackName:         stackName,
		LoadBalancerNames: []string{},
	}

	resp, _ := elbSvc.DescribeLoadBalancers(nil)
	for _, lbd := range resp.LoadBalancerDescriptions {
		loadBalancerName := aws.StringValue(lbd.LoadBalancerName)
		describeTagsInput := &elb.DescribeTagsInput{
			LoadBalancerNames: []*string{
				lbd.LoadBalancerName,
			},
		}

		tagsOutput, err := elbSvc.DescribeTags(describeTagsInput)
		if err != nil {
			return fmt.Errorf("Unable to describe loadBalancer tags: %s", err.Error())
		}

		tagInfos := map[string]string{}
		for _, tagDescription := range tagsOutput.TagDescriptions {
			for _, tag := range tagDescription.Tags {
				tagInfos[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
			}
		}

		if tagInfos["KubernetesCluster"] == stackName {
			if tagInfos["kubernetes.io/service-name"] == "kube-system/apiserver-public" {
				deploymentLoadBalancers.ApiServerBalancerName = loadBalancerName
			}
			deploymentLoadBalancers.LoadBalancerNames = append(deploymentLoadBalancers.LoadBalancerNames, loadBalancerName)
		}
	}

	for _, loadBalancerName := range deploymentLoadBalancers.LoadBalancerNames {
		// Need to skip delete k8s-master elb loadbancerName when update deployment
		if skipApiServer {
			if loadBalancerName == deploymentLoadBalancers.ApiServerBalancerName {
				continue
			}
		}

		deleteLoadBalancerInput := &elb.DeleteLoadBalancerInput{
			LoadBalancerName: aws.String(loadBalancerName),
		}
		if _, err := elbSvc.DeleteLoadBalancer(deleteLoadBalancerInput); err != nil {
			return fmt.Errorf("Unable to deleting loadBalancer: %s", err.Error())
		}
	}

	return nil
}

func deleteCloudFormationStack(sess *session.Session, deploymentName string,
	stackName string, log *logging.Logger) error {
	elbSvc := elb.New(sess)
	ec2Svc := ec2.New(sess)
	cfSvc := cloudformation.New(sess)

	// find k8s-node stack name
	describeStacksOutput, _ := cfSvc.DescribeStacks(nil)
	k8sNodeStackName := ""
	vpcID := ""
	clusterSecurityGroupId := ""
	for _, stack := range describeStacksOutput.Stacks {
		if strings.HasPrefix(aws.StringValue(stack.StackName), stackName+"-K8sStack") {
			k8sNodeStackName = aws.StringValue(stack.StackName)
			if vpcID == "" {
				for _, param := range stack.Parameters {
					if aws.StringValue(param.ParameterKey) == "VPCID" {
						vpcID = aws.StringValue(param.ParameterValue)
						break
					}
				}
			}
			if clusterSecurityGroupId == "" {
				for _, param := range stack.Parameters {
					if aws.StringValue(param.ParameterKey) == "ClusterSecGroup" {
						clusterSecurityGroupId = aws.StringValue(param.ParameterValue)
						break
					}
				}
			}
		} else if aws.StringValue(stack.StackName) == stackName {
			if vpcID == "" {
				for _, output := range stack.Outputs {
					if aws.StringValue(output.OutputKey) == "VPCID" {
						vpcID = aws.StringValue(output.OutputValue)
						break
					}
				}
			}
		}
	}

	// delete k8s-master/k8s-node stack
	if k8sNodeStackName != "" {
		log.Infof("Deleting k8s-master/k8s-node stack...")
		deleteStackInput := &cloudformation.DeleteStackInput{
			StackName: aws.String(k8sNodeStackName),
		}

		if _, err := cfSvc.DeleteStack(deleteStackInput); err != nil {
			log.Warningf("Unable to delete stack: %s", err.Error())
		}

		describeStacksInput := &cloudformation.DescribeStacksInput{
			StackName: aws.String(k8sNodeStackName),
		}

		if err := cfSvc.WaitUntilStackDeleteComplete(describeStacksInput); err != nil {
			log.Warningf("Unable to wait until stack is deleted: %s", err.Error())
		} else if err == nil {
			log.Infof("Delete %s stack ok...", k8sNodeStackName)
		}
	} else {
		log.Warningf("Unable to find k8s-master/k8s-node stack...")
	}

	if err := deleteClusterSecurityGroupNetworkInterfaces(ec2Svc, clusterSecurityGroupId, log); err != nil {
		log.Warningf("Unable to delete network interfaces of kubernetes cluster security group: " + err.Error())
	}

	if err := deleteLoadBalancers(elbSvc, false, stackName, log); err != nil {
		log.Warningf("Unable to delete load balancers: " + err.Error())
	}

	// delete bastion-Host EC2 instance
	log.Infof("Deleting bastion-Host EC2 instance...")
	if err := deleteBastionHost(ec2Svc, deploymentName); err != nil {
		log.Warningf("Unable to delete bastion-Host EC2 instance: %s", err.Error())
	}

	// delete bastion-host stack
	log.Infof("Deleting bastion-host stack...")
	deleteBastionHostStackInput := &cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	}

	if _, err := cfSvc.DeleteStack(deleteBastionHostStackInput); err != nil {
		log.Warningf("Unable to delete stack: %s", err.Error())
	}

	if err := waitUntilInternetGatewayDeleted(ec2Svc, deploymentName, time.Duration(3)*time.Minute, log); err != nil {
		log.Warningf("Unable to wait for internetGateway to be deleted: %s", err.Error())
	}

	// delete securityGroup
	retryTimes := 5
	log.Infof("Deleteing securityGroup...")
	for i := 1; i <= retryTimes; i++ {
		if err := deleteSecurityGroup(ec2Svc, aws.String(vpcID), log); err != nil {
			log.Warningf("Unable to delete securityGroup: %s, retrying %d time", err.Error(), i)
		} else if err == nil {
			log.Infof("Delete securityGroup ok...")
			break
		}
		time.Sleep(time.Duration(30) * time.Second)
	}

	describeBastionHostStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	}

	if err := cfSvc.WaitUntilStackDeleteComplete(describeBastionHostStacksInput); err != nil {
		log.Warningf("Unable to wait until stack is deleted: %s", err.Error())
	} else if err == nil {
		log.Infof("Delete %s stack ok...", stackName)
	}

	return nil
}

func deleteBastionHost(ec2Svc *ec2.EC2, deploymentName string) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("bastion-host"),
				},
			},
			{
				Name: aws.String("tag:deployment"),
				Values: []*string{
					aws.String(deploymentName),
				},
			},
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
				},
			},
		},
	}

	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	var instanceIds []*string
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			instanceIds = append(instanceIds, instance.InstanceId)
		}
	}

	params := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	}

	if _, err := ec2Svc.TerminateInstances(params); err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s\n", err.Error())
	}

	terminatedInstanceParams := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceTerminated(terminatedInstanceParams); err != nil {
		return fmt.Errorf("Unable to wait until EC2 instance terminated: %s\n", err.Error())
	}
	return nil
}

func deleteK8S(namespaces []string, kubeConfig *rest.Config, log *logging.Logger) error {
	if kubeConfig == nil {
		return errors.New("Empty kubeconfig passed, skipping to delete k8s objects")
	}

	log.Info("Found kube config, deleting kubernetes objects")
	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	for _, namespace := range namespaces {
		log.Info("Deleting kubernetes objects in namespace " + namespace)
		daemonsets := k8sClient.Extensions().DaemonSets(namespace)
		if daemonsetList, listError := daemonsets.List(metav1.ListOptions{}); listError == nil {
			for _, daemonset := range daemonsetList.Items {
				name := daemonset.GetObjectMeta().GetName()
				if err := daemonsets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete daemonset %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list daemonsets in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		deploys := k8sClient.Extensions().Deployments(namespace)
		if deployLists, listError := deploys.List(metav1.ListOptions{}); listError == nil {
			for _, deployment := range deployLists.Items {
				name := deployment.GetObjectMeta().GetName()
				if err := deploys.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete deployment %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list deployments in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		replicaSets := k8sClient.Extensions().ReplicaSets(namespace)
		if replicaSetList, listError := replicaSets.List(metav1.ListOptions{}); listError == nil {
			for _, replicaSet := range replicaSetList.Items {
				name := replicaSet.GetObjectMeta().GetName()
				if err := replicaSets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					glog.Warningf("Unable to delete replica set %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list replica sets in namespace '%s' for deletion: ", namespace, listError.Error())
		}

		services := k8sClient.CoreV1().Services(namespace)
		if serviceLists, listError := services.List(metav1.ListOptions{}); listError == nil {
			for _, service := range serviceLists.Items {
				serviceName := service.GetObjectMeta().GetName()
				if err := services.Delete(serviceName, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", serviceName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list services in namespace '%s' for deletion: %s", namespace, listError.Error())
		}

		pods := k8sClient.CoreV1().Pods(namespace)
		if podLists, listError := pods.List(metav1.ListOptions{}); listError == nil {
			for _, pod := range podLists.Items {
				podName := pod.GetObjectMeta().GetName()
				if err := pods.Delete(podName, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete pod %s: %s", podName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list pods in namespace '%s' for deletion: %s", namespace, listError.Error())
		}

		secrets := k8sClient.CoreV1().Secrets(namespace)
		if secretList, listError := secrets.List(metav1.ListOptions{}); listError == nil {
			for _, secret := range secretList.Items {
				name := secret.GetObjectMeta().GetName()
				if err := secrets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list secrets in namespace '%s' for deletion: %s", namespace, listError.Error())
		}
	}

	return nil
}

func deleteDeploymentOnFailure(k8sDeployer *K8SDeployer) {
	log := k8sDeployer.DeploymentLog.Logger
	if k8sDeployer.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	deleteDeployment(k8sDeployer)
}

func deleteDeployment(k8sDeployer *K8SDeployer) {
	awsCluster := k8sDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	deployment := k8sDeployer.Deployment
	kubeConfig := k8sDeployer.KubeConfig
	stackName := awsCluster.StackName()
	deploymentName := awsCluster.Name
	log := k8sDeployer.DeploymentLog.Logger

	// Deleting kubernetes deployment
	log.Infof("Deleting kubernetes deployment...")
	if err := deleteK8S(getAllDeployedNamespaces(deployment), kubeConfig, log); err != nil {
		log.Warningf("Unable to deleting kubernetes deployment: %s", err.Error())
	}

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		log.Warningf("Unable to create aws session for delete: %s", sessionErr.Error())
		return
	}

	// delete cloudformation Stack
	log.Infof("Deleting cloudformation Stack: %s", stackName)
	if err := deleteCloudFormationStack(sess, deploymentName, stackName, log); err != nil {
		log.Warningf("Unable to deleting cloudformation Stack: %s", err.Error())
	}

	ec2Svc := ec2.New(sess)

	log.Infof("Deleting KeyPair...")
	if err := awsecs.DeleteKeyPair(ec2Svc, awsCluster); err != nil {
		log.Warning("Unable to delete key pair: " + err.Error())
	}
}

func getAllDeployedNamespaces(deployment *apis.Deployment) []string {
	// Find all namespaces we deployed to
	allNamespaces := []string{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		newNamespace := ""
		if task.Deployment != nil {
			newNamespace = getNamespace(task.Deployment.ObjectMeta)
		} else if task.DaemonSet != nil {
			newNamespace = getNamespace(task.DaemonSet.ObjectMeta)
		}
		exists := false
		for _, namespace := range allNamespaces {
			if namespace == newNamespace {
				exists = true
				break
			}
		}

		if !exists {
			allNamespaces = append(allNamespaces, newNamespace)
		}
	}

	return allNamespaces
}

func tagKubeNodes(
	k8sClient *k8s.Clientset,
	awsCluster *hpaws.AWSCluster,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	nodeInfos := map[string]int{}
	for _, mapping := range deployment.NodeMapping {
		privateDnsName := awsCluster.NodeInfos[mapping.Id].Instance.PrivateDnsName
		nodeInfos[aws.StringValue(privateDnsName)] = mapping.Id
	}

	for nodeName, id := range nodeInfos {
		if node, err := k8sClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{}); err == nil {
			node.Labels["hyperpilot/node-id"] = strconv.Itoa(id)
			node.Labels["hyperpilot/deployment"] = awsCluster.Name
			if _, err := k8sClient.CoreV1().Nodes().Update(node); err == nil {
				log.Infof("Added label hyperpilot/node-id:%s to Kubernetes node %s", strconv.Itoa(id), nodeName)
			}
		} else {
			return fmt.Errorf("Unable to get Kubernetes node by name %s: %s", nodeName, err.Error())
		}
	}

	return nil
}

func getNamespace(objectMeta metav1.ObjectMeta) string {
	namespace := objectMeta.Namespace
	if namespace == "" {
		return "default"
	}

	return namespace
}

func createNamespaceIfNotExist(namespace string, existingNamespaces map[string]bool, k8sClient *k8s.Clientset) error {
	if _, ok := existingNamespaces[namespace]; !ok {
		glog.Infof("Creating new namespace %s", namespace)
		k8sNamespaces := k8sClient.CoreV1().Namespaces()
		_, err := k8sNamespaces.Create(&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})
		if err != nil {
			return fmt.Errorf("Unable to create namespace '%s': %s", namespace, err.Error())
		}
		existingNamespaces[namespace] = true
	}

	return nil
}

func createSecrets(k8sClient *k8s.Clientset, existingNamespaces map[string]bool, deployment *apis.Deployment) error {
	secrets := deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	for _, secret := range secrets {
		namespace := getNamespace(secret.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
		}

		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func createServiceForDeployment(namespace string, family string, k8sClient *k8s.Clientset,
	task apis.KubernetesTask, container v1.Container, log *logging.Logger) error {
	if len(container.Ports) == 0 {
		return nil
	}

	service := k8sClient.CoreV1().Services(namespace)
	serviceName := family
	if !strings.HasPrefix(family, serviceName) {
		serviceName = serviceName + "-" + container.Name
	}
	labels := map[string]string{"app": family}
	servicePorts := []v1.ServicePort{}
	for i, port := range container.Ports {
		newPort := v1.ServicePort{
			Port:       port.HostPort,
			TargetPort: intstr.FromInt(int(port.ContainerPort)),
			Name:       "port" + strconv.Itoa(i),
		}
		servicePorts = append(servicePorts, newPort)
	}

	internalService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeClusterIP,
			Ports:    servicePorts,
			Selector: labels,
		},
	}
	_, err := service.Create(internalService)
	if err != nil {
		return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
	}
	log.Infof("Created %s internal service", serviceName)

	// Check the type of each port opened by the container; create a loadbalancer service to expose the public port
	if task.PortTypes == nil || len(task.PortTypes) == 0 {
		return nil
	}

	for i, portType := range task.PortTypes {
		if portType != publicPortType {
			log.Infof("Skipping creating public endpoint for service %s as it's marked as private", serviceName)
			continue
		}
		// public port
		publicServiceName := serviceName + "-public" + servicePorts[i].Name
		publicService := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      publicServiceName,
				Labels:    labels,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{
					v1.ServicePort{
						Port:       servicePorts[i].Port,
						TargetPort: servicePorts[i].TargetPort,
						Name:       "public-" + servicePorts[i].Name,
					},
				},
				Selector: labels,
			},
		}
		_, err := service.Create(publicService)
		if err != nil {
			return fmt.Errorf("Unable to create public service %s: %s", publicServiceName, err)
		}

		log.Infof("Created a public service %s with port %d", publicServiceName, servicePorts[i].Port)
	}

	return nil
}

func (k8sDeployer *K8SDeployer) deployServices(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
	deployment := k8sDeployer.Deployment
	kubeConfig := k8sDeployer.KubeConfig
	log := k8sDeployer.DeploymentLog.Logger

	if kubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	tasks := map[string]apis.KubernetesTask{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	taskCount := map[string]int{}

	// We sort before we create services because we want to have a deterministic way to assign
	// service ids
	sort.Sort(deployment.NodeMapping)

	for _, mapping := range deployment.NodeMapping {
		log.Infof("Deploying task %s with mapping %d", mapping.Task, mapping.Id)

		task, ok := tasks[mapping.Task]
		if !ok {
			return fmt.Errorf("Unable to find task %s in task definitions", mapping.Task)
		}

		deploySpec := task.Deployment
		if deploySpec == nil {
			return fmt.Errorf("Unable to find deployment in task %s", mapping.Task)
		}
		family := task.Family
		namespace := getNamespace(deploySpec.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		originalFamily := family
		count, ok := taskCount[family]

		if !ok {
			count = 1
			deploySpec.Name = originalFamily
			deploySpec.Labels["app"] = originalFamily
			deploySpec.Spec.Template.Labels["app"] = originalFamily
		} else {
			// Update deploy spec to reflect multiple count of the same task
			count += 1
			family = family + "-" + strconv.Itoa(count)
			deploySpec.Name = family
			deploySpec.Labels["app"] = family
			deploySpec.Spec.Template.Labels["app"] = family
		}

		if deploySpec.Spec.Selector != nil {
			deploySpec.Spec.Selector.MatchLabels = deploySpec.Spec.Template.Labels
		}

		// Public Url will be tagged later in recordPublicEndpoint post deployment
		k8sDeployer.Services[family] = ServiceMapping{
			NodeId: mapping.Id,
		}

		taskCount[originalFamily] = count

		// Assigning Pods to Nodes
		nodeSelector := map[string]string{}
		log.Infof("Selecting node %d for deployment %s", mapping.Id, family)
		nodeSelector["hyperpilot/node-id"] = strconv.Itoa(mapping.Id)

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := createServiceForDeployment(namespace, family, k8sClient, task, container, log)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		deploy := k8sClient.Extensions().Deployments(namespace)
		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unable to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		if task.Deployment != nil {
			return fmt.Errorf("Cannot assign both daemonset and deployment to the same task: %s", task.Family)
		}

		daemonSet := task.DaemonSet
		namespace := getNamespace(daemonSet.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	clusterRoleBindings := k8sClient.RbacV1beta1().ClusterRoleBindings()
	hyperpilotRoleBinding := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "hyperpilot-cluster-role"},
		RoleRef: rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbac.Subject{
			rbac.Subject{
				Kind:      rbac.ServiceAccountKind,
				Name:      "default",
				Namespace: "hyperpilot",
			},
		},
	}
	if _, err := clusterRoleBindings.Create(hyperpilotRoleBinding); err != nil {
		log.Warningf("Unable to create hyperpilot role binding: " + err.Error())
	}

	return nil
}

func (k8sDeployer *K8SDeployer) recordPublicEndpoints(k8sClient *k8s.Clientset) {
	deployment := k8sDeployer.Deployment
	log := k8sDeployer.DeploymentLog.Logger

	allNamespaces := getAllDeployedNamespaces(deployment)
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		tagElbFunc := func() {
			allElbsTagged := true
			for _, namespace := range allNamespaces {
				services := k8sClient.CoreV1().Services(namespace)
				serviceLists, listError := services.List(metav1.ListOptions{})
				if listError != nil {
					log.Warningf("Unable to list services for namespace '%s': %s", namespace, listError.Error())
					return
				}
				for _, service := range serviceLists.Items {
					serviceName := service.GetObjectMeta().GetName()
					if strings.Index(serviceName, "-public") != -1 {
						if len(service.Status.LoadBalancer.Ingress) > 0 {
							hostname := service.Status.LoadBalancer.Ingress[0].Hostname
							port := service.Spec.Ports[0].Port

							serviceMapping := ServiceMapping{
								PublicUrl: hostname + ":" + strconv.FormatInt(int64(port), 10),
							}

							familyName := serviceName[:strings.Index(serviceName, "-public")]
							if mapping, ok := k8sDeployer.Services[familyName]; ok {
								serviceMapping.NodeId = mapping.NodeId
							}
							k8sDeployer.Services[familyName] = serviceMapping
						} else {
							allElbsTagged = false
							break
						}
					}
				}

				if allElbsTagged {
					c <- true
				} else {
					time.Sleep(time.Second * 2)
				}
			}
		}

		for {
			select {
			case <-quit:
				return
			default:
				tagElbFunc()
			}
		}
	}()

	select {
	case <-c:
		log.Info("All public endpoints recorded.")
	case <-time.After(time.Duration(2) * time.Minute):
		quit <- true
		log.Warning("Timed out waiting for AWS ELB to be ready.")
	}
}

// DownloadKubeConfig is Use SSHProxyCommand download k8s master node's kubeconfig
func (k8sDeployer *K8SDeployer) DownloadKubeConfig() error {
	awsCluster := k8sDeployer.AWSCluster

	baseDir := awsCluster.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	clientConfig, clientConfigErr := awsCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	sshClient := common.NewSshClient(k8sDeployer.MasterIp+":22", clientConfig, k8sDeployer.BastionIp+":22")
	if err := sshClient.CopyRemoteFileToLocal("/home/ubuntu/kubeconfig", kubeconfigFilePath); err != nil {
		return errors.New("Unable to copy kubeconfig file to local: " + err.Error())
	}

	k8sDeployer.KubeConfigPath = kubeconfigFilePath
	return nil
}

// UploadSshKeyToBastion upload sshKey to bastion-host
func (k8sDeployer *K8SDeployer) UploadSshKeyToBastion() error {
	awsCluster := k8sDeployer.AWSCluster

	baseDir := awsCluster.Name + "_sshkey"
	basePath := "/tmp/" + baseDir
	sshKeyFilePath := basePath + "/" + awsCluster.KeyName() + ".pem"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(sshKeyFilePath)

	privateKey := strings.Replace(*awsCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
	if err := ioutil.WriteFile(sshKeyFilePath, []byte(privateKey), 0400); err != nil {
		return fmt.Errorf("Unable to create %s sshKey file: %s",
			awsCluster.Name, err.Error())
	}

	clientConfig, err := awsCluster.SshConfig("ubuntu")
	if err != nil {
		return errors.New("Unable to create ssh config: " + err.Error())
	}

	address := k8sDeployer.BastionIp + ":22"
	scpClient := common.NewSshClient(address, clientConfig, "")

	remotePath := "/home/ubuntu/" + awsCluster.KeyName() + ".pem"
	if err := scpClient.CopyLocalFileToRemote(sshKeyFilePath, remotePath); err != nil {
		errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
			awsCluster.KeyName(), address, err.Error())
		return errors.New(errorMsg)
	}

	return nil
}

func (k8sDeployer *K8SDeployer) GetAWSCluster() *hpaws.AWSCluster {
	return k8sDeployer.AWSCluster
}

// CheckClusterState check kubernetes cluster state is exist
func (k8sDeployer *K8SDeployer) CheckClusterState() error {
	awsCluster := k8sDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	cfSvc := cloudformation.New(sess)

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(awsCluster.StackName()),
	}

	describeStacksOutput, err := cfSvc.DescribeStacks(describeStacksInput)
	if err != nil {
		return errors.New("Unable to get stack outputs: " + err.Error())
	}

	stackStatus := aws.StringValue(describeStacksOutput.Stacks[0].StackStatus)
	if stackStatus != "CREATE_COMPLETE" {
		return errors.New("Unable to reload stack because status is not ready, current status: " + stackStatus)
	}

	return nil
}

// ReloadClusterState reloads kubernetes cluster state
func (k8sDeployer *K8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	deploymentName := k8sDeployer.AWSCluster.Name
	if err := k8sDeployer.CheckClusterState(); err != nil {
		return fmt.Errorf("Skipping reloading because unable to load %s stack: %s", deploymentName, err.Error())
	}

	k8sStoreInfo := storeInfo.(*StoreInfo)
	k8sDeployer.BastionIp = k8sStoreInfo.BastionIp
	k8sDeployer.MasterIp = k8sStoreInfo.MasterIp

	if err := k8sDeployer.DownloadKubeConfig(); err != nil {
		return fmt.Errorf("Unable to download %s kubeconfig: %s", deploymentName, err.Error())
	}
	glog.Infof("Downloaded %s kube config at %s", k8sDeployer.AWSCluster.Name, k8sDeployer.KubeConfigPath)

	kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployer.KubeConfigPath)
	if err != nil {
		return fmt.Errorf("Unable to parse %s kube config: %s", k8sDeployer.AWSCluster.Name, err.Error())
	}
	k8sDeployer.KubeConfig = kubeConfig

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}
	k8sDeployer.recordPublicEndpoints(k8sClient)

	return nil
}

func (k8sDeployer *K8SDeployer) GetClusterInfo() (*ClusterInfo, error) {
	kubeConfig := k8sDeployer.KubeConfig
	if kubeConfig == nil {
		return nil, errors.New("Empty kubeconfig passed, skipping to get k8s objects")
	}

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}

	nodes := k8sClient.CoreV1().Nodes()
	nodeLists, nodeError := nodes.List(metav1.ListOptions{})
	if nodeError != nil {
		return nil, fmt.Errorf("Unable to list nodes for get cluster: %s", nodeError.Error())
	}

	pods := k8sClient.CoreV1().Pods("")
	podLists, podError := pods.List(metav1.ListOptions{})
	if podError != nil {
		return nil, fmt.Errorf("Unable to list pods for get cluster: %s", podError.Error())
	}

	deploys := k8sClient.Extensions().Deployments("")
	deployLists, depError := deploys.List(metav1.ListOptions{})
	if depError != nil {
		return nil, fmt.Errorf("Unable to list deployments for get cluster: %s", depError.Error())
	}

	clusterInfo := &ClusterInfo{
		Nodes:      nodeLists.Items,
		Pods:       podLists.Items,
		Containers: deployLists.Items,
		BastionIp:  k8sDeployer.BastionIp,
		MasterIp:   k8sDeployer.MasterIp,
	}

	return clusterInfo, nil
}

func (k8sDeployer *K8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	nodeNameInfos := map[string]string{}
	if len(k8sDeployer.AWSCluster.NodeInfos) > 0 {
		for id, nodeInfo := range k8sDeployer.AWSCluster.NodeInfos {
			nodeNameInfos[strconv.Itoa(id)] = aws.StringValue(nodeInfo.Instance.PrivateDnsName)
		}
	} else {
		k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
		if err != nil {
			return nil, errors.New("Unable to connect to Kubernetes: " + err.Error())
		}

		nodes, nodeError := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if nodeError != nil {
			return nil, fmt.Errorf("Unable to list nodes: %s", nodeError.Error())
		}

		for _, node := range nodes.Items {
			nodeNameInfos[node.Labels["hyperpilot/node-id"]] = node.Name
		}
	}

	serviceMappings := make(map[string]interface{})
	for serviceName, serviceMapping := range k8sDeployer.Services {
		if serviceMapping.NodeId == 0 {
			serviceNodeId, err := k8sDeployer.findNodeIdFromServiceName(serviceName)
			if err != nil {
				return nil, fmt.Errorf("Unable to find %s node id: %s", serviceName, err.Error())
			}
			serviceMapping.NodeId = serviceNodeId
		}
		serviceMapping.NodeName = nodeNameInfos[strconv.Itoa(serviceMapping.NodeId)]
		serviceMappings[serviceName] = serviceMapping
	}

	return serviceMappings, nil
}

// findNodeIdFromServiceName finds the node id that should be running this service
func (k8sDeployer *K8SDeployer) findNodeIdFromServiceName(serviceName string) (int, error) {
	// if a service name contains a number (e.g: benchmark-agent-2), we assume
	// it's the second benchmark agent from the mapping. Since we should be sorting
	// the node ids when we deploy them, we should always assign the same service name
	// for the same app running on the same node.
	parts := strings.Split(serviceName, "-")
	count := 1
	realServiceName := serviceName
	if len(parts) > 0 {
		if nth, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			count = nth
			realServiceName = strings.Join(parts[:len(parts)-1], "-")
		}
	}
	sort.Sort(k8sDeployer.Deployment.NodeMapping)
	current := 0
	for _, mapping := range k8sDeployer.Deployment.NodeMapping {
		if mapping.Task == realServiceName {
			current += 1
			if current == count {
				return mapping.Id, nil
			}
		}
	}

	return 0, errors.New("Unable to find service in mappings")
}

// GetServiceAddress return ServiceAddress object
func (k8sDeployer *K8SDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	services, err := k8sClient.CoreV1().Services("").List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("Unable to list services in the cluster: " + err.Error())
	}

	for _, service := range services.Items {
		if (service.ObjectMeta.Name == serviceName || service.ObjectMeta.Name == serviceName+"-publicport0") &&
			string(service.Spec.Type) == "LoadBalancer" {
			port := service.Spec.Ports[0].Port
			hostname := service.Status.LoadBalancer.Ingress[0].Hostname
			address := &apis.ServiceAddress{Host: hostname, Port: port}

			return address, nil
		}
	}

	return nil, errors.New("Service not found in endpoints")
}

func (k8sDeployer *K8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	if info, ok := k8sDeployer.Services[serviceName]; ok {
		return info.PublicUrl, nil
	}

	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return "", errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	services, err := k8sClient.CoreV1().Services("").List(metav1.ListOptions{})
	if err != nil {
		return "", errors.New("Unable to list services in the cluster: " + err.Error())
	}

	for _, service := range services.Items {
		if (service.ObjectMeta.Name == serviceName || service.ObjectMeta.Name == serviceName+"-publicport0") &&
			string(service.Spec.Type) == "LoadBalancer" {
			nodeId, _ := k8sDeployer.findNodeIdFromServiceName(serviceName)
			port := service.Spec.Ports[0].Port
			hostname := service.Status.LoadBalancer.Ingress[0].Hostname
			serviceUrl := hostname + ":" + strconv.FormatInt(int64(port), 10)
			k8sDeployer.Services[serviceName] = ServiceMapping{
				PublicUrl: serviceUrl,
				NodeId:    nodeId,
			}
			return serviceUrl, nil
		}
	}

	return "", errors.New("Service not found in endpoints")
}

func (k8sDeployer *K8SDeployer) GetStoreInfo() interface{} {
	return &StoreInfo{
		BastionIp: k8sDeployer.BastionIp,
		MasterIp:  k8sDeployer.MasterIp,
	}
}
