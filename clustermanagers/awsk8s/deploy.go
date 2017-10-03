package awsk8s

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	k8sUtil "github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/clusters"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
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
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewDeployer return the K8S of Deployer
func NewDeployer(
	config *viper.Viper,
	cluster clusters.Cluster,
	awsProfile *hpaws.AWSProfile,
	deployment *apis.Deployment) (*K8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	deployer := &K8SDeployer{
		Config:        config,
		AWSCluster:    cluster.(*hpaws.AWSCluster),
		Deployment:    deployment,
		DeploymentLog: log,
		Services:      make(map[string]ServiceMapping),
	}

	return deployer, nil
}

func (deployer *K8SDeployer) GetLog() *log.FileLog {
	return deployer.DeploymentLog
}

func (deployer *K8SDeployer) GetScheduler() *job.Scheduler {
	return deployer.Scheduler
}

func (deployer *K8SDeployer) SetScheduler(sheduler *job.Scheduler) {
	deployer.Scheduler = sheduler
}

func (deployer *K8SDeployer) GetKubeConfigPath() (string, error) {
	return deployer.KubeConfigPath, nil
}

// CreateDeployment start a deployment
func (deployer *K8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	if err := deployCluster(deployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	response := &CreateDeploymentResponse{
		Name:      deployer.Deployment.Name,
		Services:  deployer.Services,
		BastionIp: deployer.BastionIp,
		MasterIp:  deployer.MasterIp,
	}

	return response, nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (deployer *K8SDeployer) UpdateDeployment(deployment *apis.Deployment) error {
	deployer.Deployment = deployment
	awsCluster := deployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	stackName := awsCluster.StackName()
	log := deployer.DeploymentLog.Logger

	log.Info("Updating kubernetes deployment")
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := k8sUtil.DeleteK8S(k8sUtil.GetAllDeployedNamespaces(deployment), deployer.KubeConfig, log); err != nil {
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

	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, log); err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}
	deployer.recordPublicEndpoints(k8sClient)

	return nil
}

func (deployer *K8SDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes: " + err.Error())
	}

	originalDeployment := deployer.Deployment
	deployer.Deployment = extensions
	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient,
		deployer.Deployment, deployer.GetLog().Logger); err != nil {
		deployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}

	deployer.Deployment = newDeployment
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (deployer *K8SDeployer) DeleteDeployment() error {
	awsCluster := deployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	deployment := deployer.Deployment
	kubeConfig := deployer.KubeConfig
	stackName := awsCluster.StackName()
	deploymentName := awsCluster.Name
	log := deployer.DeploymentLog.Logger

	// Deleting kubernetes deployment
	log.Infof("Deleting kubernetes deployment...")
	if err := k8sUtil.DeleteK8S(k8sUtil.GetAllDeployedNamespaces(deployment), kubeConfig, log); err != nil {
		log.Warningf("Unable to deleting kubernetes deployment: %s", err.Error())
	}

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		log.Warningf("Unable to create aws session for delete: %s", sessionErr.Error())
		return nil
	}

	ec2Svc := ec2.New(sess)
	if deployer.VpcPeeringConnectionId != "" {
		_, err := ec2Svc.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{
			VpcPeeringConnectionId: aws.String(deployer.VpcPeeringConnectionId),
		})
		if err != nil {
			log.Warningf("Unable to remove Vpc peering: " + err.Error())
		}
	}

	// delete cloudformation Stack
	log.Infof("Deleting cloudformation Stack: %s", stackName)
	if err := deleteCloudFormationStack(sess, deploymentName, stackName, log); err != nil {
		log.Warningf("Unable to deleting cloudformation Stack: %s", err.Error())
	}

	log.Infof("Deleting KeyPair...")
	if err := awsecs.DeleteKeyPair(ec2Svc, awsCluster); err != nil {
		log.Warning("Unable to delete key pair: " + err.Error())
	}

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

func deployCluster(deployer *K8SDeployer, uploadedFiles map[string]string) error {
	deployment := deployer.Deployment
	awsCluster := deployer.AWSCluster
	awsProfile := awsCluster.AWSProfile
	log := deployer.DeploymentLog.Logger

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

	if err := deployKubernetes(sess, deployer); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := populateNodeInfos(ec2Svc, awsCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := deployer.uploadFiles(uploadedFiles); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, awsCluster, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}
	deployer.recordPublicEndpoints(k8sClient)

	return nil
}

func deployKubernetes(sess *session.Session, deployer *K8SDeployer) error {
	awsCluster := deployer.AWSCluster
	deployment := deployer.Deployment
	log := deployer.DeploymentLog.Logger

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
				ParameterValue: aws.String(awsCluster.Region + "a"),
			},
		},
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String("deployment"),
				Value: aws.String(awsCluster.Name),
			},
		},
		TemplateURL:      aws.String("https://hyperpilot-snap-collectors.s3.amazonaws.com/kubernetes-cluster-with-new-vpc-1.7.2.template"),
		TimeoutInMinutes: aws.Int64(60),
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
	vpcId := ""
	for _, output := range outputs {
		switch *output.OutputKey {
		case "SSHProxyCommand":
			sshProxyCommand = *output.OutputValue
		case "GetKubeConfigCommand":
			getKubeConfigCommand = *output.OutputValue
		case "VPCID":
			vpcId = *output.OutputValue
		}
	}

	if sshProxyCommand == "" {
		return errors.New("Unable to find SSHProxyCommand in stack output")
	} else if getKubeConfigCommand == "" {
		return errors.New("Unable to find GetKubeConfigCommand in stack output")
	} else if vpcId == "" {
		return errors.New("Unable to find VPCID in stack output")
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

	deployer.BastionIp = addresses[0]
	deployer.MasterIp = addresses[1]

	if err := deployer.DownloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}

	log.Infof("Downloaded kube config at %s", deployer.KubeConfigPath)

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", deployer.KubeConfigPath); err != nil {
		return errors.New("Unable to parse kube config: " + err.Error())
	} else {
		deployer.KubeConfig = kubeConfig
	}

	if err := deployer.UploadSshKeyToBastion(); err != nil {
		return errors.New("Unable to upload sshKey: " + err.Error())
	}

	if deployer.Deployment.VPCPeering != nil {
		ec2Svc := ec2.New(sess)
		vpcPeering, err := ec2Svc.CreateVpcPeeringConnection(&ec2.CreateVpcPeeringConnectionInput{
			PeerOwnerId: aws.String(deployer.Deployment.VPCPeering.TargetOwnerId),
			PeerVpcId:   aws.String(deployer.Deployment.VPCPeering.TargetVpcId),
			VpcId:       aws.String(vpcId),
		})
		if err != nil {
			return errors.New("Unable to peer vpc: " + err.Error())
		}

		vpcPeeringConnectionId := vpcPeering.VpcPeeringConnection.VpcPeeringConnectionId
		deployer.VpcPeeringConnectionId = *vpcPeeringConnectionId

		err = acceptVpcPeeringConnection(vpcPeeringConnectionId, deployer.Config,
			deployer.Deployment.Region)
		if err != nil {
			return errors.New("Unable to auto accept peer vpc: " + err.Error())
		}
	}

	return nil
}

func acceptVpcPeeringConnection(vpcPeeringConnectionId *string, config *viper.Viper, region string) error {
	awsProfile := &hpaws.AWSProfile{
		AwsId:     config.GetString("awsId"),
		AwsSecret: config.GetString("awsSecret"),
	}

	sess, sessionErr := hpaws.CreateSession(awsProfile, region)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	_, err := ec2Svc.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{
		VpcPeeringConnectionId: vpcPeeringConnectionId,
	})
	if err != nil {
		return errors.New("Unable to accept vpc peering connection: " + err.Error())
	}

	return nil
}

func waitUntilNetworkInterfacesAvailable(ec2Svc *ec2.EC2, filters []*ec2.Filter,
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

func waitUntilKubernetesInstanceTerminated(ec2Svc *ec2.EC2, stackName string, log *logging.Logger) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:KubernetesCluster"),
				Values: []*string{aws.String(stackName)},
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

	_, err := ec2Svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	})
	if err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s", err.Error())
	}

	err = ec2Svc.WaitUntilInstanceTerminated(&ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	})
	if err != nil {
		return fmt.Errorf("Unable to wait until EC2 instance terminated: %s\n", err.Error())
	}

	return nil
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

	if err := waitUntilNetworkInterfacesAvailable(ec2Svc, filters, time.Duration(60)*time.Second, log); err != nil {
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
	stackName string, log *logging.Logger) error {
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:KubernetesCluster"),
				Values: []*string{aws.String(stackName)},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []*string{aws.String("k8s-cluster-security-group")},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	if len(resp.SecurityGroups) > 0 {
		filters := []*ec2.Filter{&ec2.Filter{
			Name:   aws.String("group-id"),
			Values: []*string{resp.SecurityGroups[0].GroupId},
		}}

		if err := deleteNetworkInterfaces(ec2Svc, filters, log); err != nil {
			return fmt.Errorf("Unable to delete network interfaces: %s", err.Error())
		}
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

	log.Infof("Deleting %s stack...", stackName)
	_, err := cfSvc.DeleteStack(&cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		log.Warningf("Unable to delete stack: %s", err.Error())
	}

	log.Infof("Deleting %s load balancers...", stackName)
	if err := deleteLoadBalancers(elbSvc, false, stackName, log); err != nil {
		log.Warningf("Unable to delete load balancers: " + err.Error())
	}

	log.Infof("Waiting until %s kubernetes instance is terminated...", stackName)
	if err := waitUntilKubernetesInstanceTerminated(ec2Svc, stackName, log); err != nil {
		log.Warningf("Unable wait for k8s-node ec2 instance to be terminated: " + err.Error())
	}

	log.Infof("Deleting %s network interfaces of clusterSecurityGroup...", stackName)
	if err := deleteClusterSecurityGroupNetworkInterfaces(ec2Svc, stackName, log); err != nil {
		log.Warningf("Unable to delete network interfaces of kubernetes cluster security group: " + err.Error())
	}

	log.Infof("Deleting %s elb security group...", stackName)
	if err := deleteElbSecurityGroup(ec2Svc, stackName, log); err != nil {
		log.Warningf("Unable to deleting %s elb securityGroups: %s", stackName, err.Error())
	}

	log.Infof("Waiting until %s stack to be delete completed...", stackName)
	err = cfSvc.WaitUntilStackDeleteComplete(&cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		return fmt.Errorf("Unable to wait until %s stack to be delete completed: %s", stackName, err.Error())
	}
	log.Infof("Delete %s stack ok...", stackName)

	return nil
}

func deleteDeploymentOnFailure(deployer *K8SDeployer) {
	log := deployer.DeploymentLog.Logger
	if deployer.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	deployer.DeleteDeployment()
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

	return k8sUtil.TagKubeNodes(k8sClient, deployment.Name, nodeInfos, log)
}

func (deployer *K8SDeployer) recordPublicEndpoints(k8sClient *k8s.Clientset) {
	deployment := deployer.Deployment
	log := deployer.DeploymentLog.Logger
	deployer.Services = map[string]ServiceMapping{}

	allNamespaces := k8sUtil.GetAllDeployedNamespaces(deployment)
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
							if mapping, ok := deployer.Services[familyName]; ok {
								serviceMapping.NodeId = mapping.NodeId
							}
							deployer.Services[familyName] = serviceMapping
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
func (deployer *K8SDeployer) DownloadKubeConfig() error {
	awsCluster := deployer.AWSCluster

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

	sshClient := common.NewSshClient(deployer.MasterIp+":22", clientConfig, deployer.BastionIp+":22")
	if err := sshClient.CopyRemoteFileToLocal("/home/ubuntu/kubeconfig", kubeconfigFilePath); err != nil {
		return errors.New("Unable to copy kubeconfig file to local: " + err.Error())
	}

	deployer.KubeConfigPath = kubeconfigFilePath
	return nil
}

func (deployer *K8SDeployer) uploadFiles(uploadedFiles map[string]string) error {
	awsCluster := deployer.AWSCluster
	deployment := deployer.Deployment
	bastionIp := deployer.BastionIp
	log := deployer.GetLog().Logger
	if len(deployment.Files) == 0 {
		return nil
	}

	clientConfig, clientConfigErr := awsCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	var errBool bool
	for _, nodeInfo := range awsCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PrivateIp+":22", clientConfig, bastionIp+":22")
		if err := common.UploadFiles(sshClient, deployment, uploadedFiles, log); err != nil {
			errBool = true
		}
	}
	if errBool {
		return errors.New("Unable to uploaded all files")
	}
	log.Info("Uploaded all files")

	return nil
}

// UploadSshKeyToBastion upload sshKey to bastion-host
func (deployer *K8SDeployer) UploadSshKeyToBastion() error {
	awsCluster := deployer.AWSCluster

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

	address := deployer.BastionIp + ":22"
	scpClient := common.NewSshClient(address, clientConfig, "")

	remotePath := "/home/ubuntu/" + awsCluster.KeyName() + ".pem"
	if err := scpClient.CopyLocalFileToRemote(sshKeyFilePath, remotePath); err != nil {
		errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
			awsCluster.KeyName(), address, err.Error())
		return errors.New(errorMsg)
	}

	return nil
}

func (deployer *K8SDeployer) GetCluster() clusters.Cluster {
	return deployer.AWSCluster
}

// CheckClusterState check kubernetes cluster state is exist
func (deployer *K8SDeployer) CheckClusterState() error {
	awsCluster := deployer.AWSCluster
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
func (deployer *K8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	deploymentName := deployer.AWSCluster.Name
	if err := deployer.CheckClusterState(); err != nil {
		return fmt.Errorf("Skipping reloading because unable to load %s stack: %s", deploymentName, err.Error())
	}

	k8sStoreInfo := storeInfo.(*StoreInfo)
	deployer.BastionIp = k8sStoreInfo.BastionIp
	deployer.MasterIp = k8sStoreInfo.MasterIp
	deployer.VpcPeeringConnectionId = k8sStoreInfo.VpcPeeringConnectionId

	glog.Infof("Reloading kube config for %s...", deployer.AWSCluster.Name)
	if err := deployer.DownloadKubeConfig(); err != nil {
		return fmt.Errorf("Unable to download %s kubeconfig: %s", deploymentName, err.Error())
	}
	glog.Infof("Reloaded %s kube config at %s", deployer.AWSCluster.Name, deployer.KubeConfigPath)

	kubeConfig, err := clientcmd.BuildConfigFromFlags("", deployer.KubeConfigPath)
	if err != nil {
		return fmt.Errorf("Unable to parse %s kube config: %s", deployer.AWSCluster.Name, err.Error())
	}
	deployer.KubeConfig = kubeConfig

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}
	deployer.recordPublicEndpoints(k8sClient)

	return nil
}

func (deployer *K8SDeployer) GetClusterInfo() (*ClusterInfo, error) {
	kubeConfig := deployer.KubeConfig
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
		BastionIp:  deployer.BastionIp,
		MasterIp:   deployer.MasterIp,
	}

	return clusterInfo, nil
}

func (deployer *K8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	nodeNameInfos := map[string]string{}
	if len(deployer.AWSCluster.NodeInfos) > 0 {
		for id, nodeInfo := range deployer.AWSCluster.NodeInfos {
			nodeNameInfos[strconv.Itoa(id)] = aws.StringValue(nodeInfo.Instance.PrivateDnsName)
		}
	} else {
		k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
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
	for serviceName, serviceMapping := range deployer.Services {
		if serviceMapping.NodeId == 0 {
			serviceNodeId, err := k8sUtil.FindNodeIdFromServiceName(deployer.Deployment, serviceName)
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

// GetServiceAddress return ServiceAddress object
func (deployer *K8SDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
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

func (deployer *K8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	if info, ok := deployer.Services[serviceName]; ok {
		return info.PublicUrl, nil
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
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
			nodeId, _ := k8sUtil.FindNodeIdFromServiceName(deployer.Deployment, serviceName)
			port := service.Spec.Ports[0].Port
			hostname := service.Status.LoadBalancer.Ingress[0].Hostname
			serviceUrl := hostname + ":" + strconv.FormatInt(int64(port), 10)
			deployer.Services[serviceName] = ServiceMapping{
				PublicUrl: serviceUrl,
				NodeId:    nodeId,
			}
			return serviceUrl, nil
		}
	}

	return "", errors.New("Service not found in endpoints")
}

func (deployer *K8SDeployer) GetStoreInfo() interface{} {
	return &StoreInfo{
		BastionIp:              deployer.BastionIp,
		MasterIp:               deployer.MasterIp,
		VpcPeeringConnectionId: deployer.VpcPeeringConnectionId,
	}
}

func (deployer *K8SDeployer) NewStoreInfo() interface{} {
	return &StoreInfo{}
}
