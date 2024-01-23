package utils

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"strings"
	"sync"
	"time"
)

type UpgradeDataType struct {
	sync.Mutex
	RunnerUpgradeDockerImage     string
	ControllerUpgradeDockerImage string
	UpgradeFromTs                int64
	UpgradeToTs                  int64
}

func (u *UpgradeDataType) Set(runnerUpgradeDockerImage, controllerUpgradeDockerImage string, upgradeFromTs, upgradeToTs int64) {
	u.Lock()
	defer u.Unlock()
	u.UpgradeFromTs = upgradeFromTs
	u.UpgradeToTs = upgradeToTs
	u.RunnerUpgradeDockerImage = runnerUpgradeDockerImage
	u.ControllerUpgradeDockerImage = controllerUpgradeDockerImage
}

func (u *UpgradeDataType) Get() (runnerUpgradeDockerImage, controllerUpgradeDockerImage string, upgradeFromTs, upgradeToTs int64) {
	u.Lock()
	defer u.Unlock()
	runnerUpgradeDockerImage = u.RunnerUpgradeDockerImage
	controllerUpgradeDockerImage = u.ControllerUpgradeDockerImage
	upgradeFromTs = u.UpgradeFromTs
	upgradeToTs = u.UpgradeToTs
	return
}

var UpgradeData = UpgradeDataType{}

func registerDeploymentRunnerTaskDefinition(ecsClient *ecs.Client, service, organizationId, token, region, dockerImage,
	osStr, cpuStr, memory, taskExecutionRoleArn, taskRoleArn, awsAccountID string) (taskDefinitionArn string, err error) {

	osCpuStr := fmt.Sprintf("%s%s", osStr, cpuStr)
	runnerName := fmt.Sprintf("dr-%s-%s-%s", osCpuStr, organizationId, region)

	tags := strings.Split(dockerImage, ":")
	tag := "unknown"
	if len(tags) == 2 {
		tag = tags[1]
	}

	logsStreamPrefix := tag
	logGroupName := fmt.Sprintf("dr-logs-group-%s", osCpuStr)

	envVars := []ecsTypes.KeyValuePair{
		{
			Name:  aws.String("Token"),
			Value: aws.String(token),
		},
		{
			Name:  aws.String("OrganizationID"),
			Value: aws.String(organizationId),
		},
		{
			Name:  aws.String("Service"),
			Value: aws.String(service),
		},
		{
			Name:  aws.String("DockerImage"),
			Value: aws.String(dockerImage),
		},
		{
			Name:  aws.String("Region"),
			Value: aws.String(region),
		},
		{
			Name:  aws.String("Memory"),
			Value: aws.String(memory),
		},
		{
			Name:  aws.String("ExecutionRoleArn"),
			Value: aws.String(taskExecutionRoleArn),
		},
		{
			Name:  aws.String("TaskRoleArn"),
			Value: aws.String(taskRoleArn),
		},
		{
			Name:  aws.String("AWSAccountID"),
			Value: aws.String(awsAccountID),
		},
	}

	containerDefinition := ecsTypes.ContainerDefinition{
		DisableNetworking: aws.Bool(false),
		Environment:       envVars,
		Essential:         aws.Bool(true),
		Image:             aws.String(dockerImage),
		Interactive:       aws.Bool(false),
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         logGroupName,
				"awslogs-region":        region,
				"awslogs-stream-prefix": logsStreamPrefix,
			},
		},
		MountPoints: []ecsTypes.MountPoint{
			{
				ContainerPath: aws.String("/var/run/docker.sock"),
				SourceVolume:  aws.String("docker-socket"),
			},
			{
				ContainerPath: aws.String("/tmp"),
				SourceVolume:  aws.String("temp"),
			},
		},
		Name:                   aws.String(runnerName),
		Privileged:             aws.Bool(false),
		PseudoTerminal:         aws.Bool(false),
		ReadonlyRootFilesystem: aws.Bool(false),
	}

	taskDefinitionFamilyName := runnerName

	cpuArch := ecsTypes.CPUArchitectureX8664

	if cpuStr == "arm" {
		cpuArch = ecsTypes.CPUArchitectureArm64
	}

	osFamily := ecsTypes.OSFamilyLinux
	if osStr == "windows" {
		osFamily = ecsTypes.OSFamilyWindowsServer2022Core
	}

	registerTaskDefinitionInput := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{
			containerDefinition,
		},
		ExecutionRoleArn: aws.String(taskExecutionRoleArn),
		Family:           aws.String(taskDefinitionFamilyName),
		Memory:           aws.String(memory),
		NetworkMode:      ecsTypes.NetworkModeHost,
		RuntimePlatform: &ecsTypes.RuntimePlatform{
			CpuArchitecture:       cpuArch,
			OperatingSystemFamily: osFamily,
		},
		Tags: []ecsTypes.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(taskDefinitionFamilyName),
			},
			{
				Key:   aws.String("created by"),
				Value: aws.String("deployment.io"),
			},
		},
		TaskRoleArn: aws.String(taskRoleArn),
		Volumes: []ecsTypes.Volume{
			{
				Host: &ecsTypes.HostVolumeProperties{SourcePath: aws.String("/var/run/docker.sock")},
				Name: aws.String("docker-socket"),
			},
			{
				Host: &ecsTypes.HostVolumeProperties{SourcePath: aws.String("/tmp")},
				Name: aws.String("temp"),
			},
		},
	}

	registerTaskDefinitionOutput, err := ecsClient.RegisterTaskDefinition(context.TODO(), registerTaskDefinitionInput)

	if err != nil {
		return "", err
	}

	taskDefinitionArn = aws.ToString(registerTaskDefinitionOutput.TaskDefinition.TaskDefinitionArn)

	return taskDefinitionArn, nil
}

func updateDeploymentRunnerService(ecsClient *ecs.Client, organizationId, osStr, cpuStr, taskDefinitionArn, region string) error {
	osCpuStr := fmt.Sprintf("%s%s", osStr, cpuStr)

	ccName := fmt.Sprintf("dr-capacity-provider-%s-%s-%s", osCpuStr, organizationId, region)
	ecsClusterName := fmt.Sprintf("dr-%s-%s", osCpuStr, organizationId)
	ecsServiceName := fmt.Sprintf("dr-%s-%s-%s", osCpuStr, organizationId, region)

	updateServiceInput := &ecs.UpdateServiceInput{
		CapacityProviderStrategy: []ecsTypes.CapacityProviderStrategyItem{{
			CapacityProvider: aws.String(ccName),
			Weight:           1,
		}},
		Cluster: aws.String(ecsClusterName),
		DeploymentConfiguration: &ecsTypes.DeploymentConfiguration{
			MaximumPercent:        aws.Int32(100),
			MinimumHealthyPercent: aws.Int32(0),
		},
		DesiredCount:         aws.Int32(0),
		EnableECSManagedTags: aws.Bool(false),
		EnableExecuteCommand: aws.Bool(false),
		PropagateTags:        ecsTypes.PropagateTagsTaskDefinition,
		Service:              aws.String(ecsServiceName),
		TaskDefinition:       aws.String(taskDefinitionArn),
	}
	_, err := ecsClient.UpdateService(context.TODO(), updateServiceInput)
	return err
}

func UpgradeDeploymentRunner(service, organizationId, token, region, runnerDockerImage, osStr, cpuStr, memory, taskExecutionRoleArn, taskRoleArn, awsAccountID string) (string, error) {
	runnerUpgradeDockerImage, _, upgradeFromTs, upgradeToTs := UpgradeData.Get()
	now := time.Now().Unix()
	if now > upgradeFromTs && now < upgradeToTs {
		if len(runnerUpgradeDockerImage) > 0 && runnerDockerImage != runnerUpgradeDockerImage {
			//upgrade deployment runner to upgraded image
			cfg, err := config.LoadDefaultConfig(context.TODO())
			if err != nil {
				return runnerDockerImage, err
			}
			ecsClient := ecs.NewFromConfig(cfg, func(o *ecs.Options) {
				o.Region = region
			})

			//register new task definition
			taskDefinitionArn, err := registerDeploymentRunnerTaskDefinition(ecsClient, service, organizationId, token,
				region, runnerUpgradeDockerImage, osStr, cpuStr, memory, taskExecutionRoleArn, taskRoleArn, awsAccountID)
			if err != nil {
				return runnerDockerImage, err
			}

			//update service
			err = updateDeploymentRunnerService(ecsClient, organizationId, osStr, cpuStr, taskDefinitionArn, region)
			if err != nil {
				return runnerDockerImage, err
			}
			return runnerUpgradeDockerImage, nil
		}
	}
	return runnerDockerImage, nil
}
