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

// RunnerTaskDefinitionConfig groups the runner task-definition parameters so the
// upgrade path stays within the repo's four-parameter limit (see CLAUDE.md Rule 2.3).
type RunnerTaskDefinitionConfig struct {
	Service              string
	OrganizationID       string
	Token                string
	Region               string
	DockerImage          string
	OsStr                string
	CpuStr               string
	Memory               string
	TaskExecutionRoleArn string
	TaskRoleArn          string
	AWSAccountID         string
	// BedrockRoleArn is the runner's Bedrock-only role ARN. It must be re-registered
	// on the runner task definition during auto-upgrade, otherwise the runner loses it
	// after the first upgrade and Bedrock silently stops working.
	BedrockRoleArn string
}

func registerDeploymentRunnerTaskDefinition(ecsClient *ecs.Client, runnerCfg RunnerTaskDefinitionConfig) (taskDefinitionArn string, err error) {

	osCpuStr := fmt.Sprintf("%s%s", runnerCfg.OsStr, runnerCfg.CpuStr)
	runnerName := fmt.Sprintf("dr-%s-%s-%s", osCpuStr, runnerCfg.OrganizationID, runnerCfg.Region)

	tags := strings.Split(runnerCfg.DockerImage, ":")
	tag := "unknown"
	if len(tags) == 2 {
		tag = tags[1]
	}

	logsStreamPrefix := tag
	logGroupName := fmt.Sprintf("dr-logs-group-%s", osCpuStr)

	envVars := []ecsTypes.KeyValuePair{
		{
			Name:  aws.String("Token"),
			Value: aws.String(runnerCfg.Token),
		},
		{
			Name:  aws.String("OrganizationID"),
			Value: aws.String(runnerCfg.OrganizationID),
		},
		{
			Name:  aws.String("Service"),
			Value: aws.String(runnerCfg.Service),
		},
		{
			Name:  aws.String("DockerImage"),
			Value: aws.String(runnerCfg.DockerImage),
		},
		{
			Name:  aws.String("Region"),
			Value: aws.String(runnerCfg.Region),
		},
		{
			Name:  aws.String("Memory"),
			Value: aws.String(runnerCfg.Memory),
		},
		{
			Name:  aws.String("ExecutionRoleArn"),
			Value: aws.String(runnerCfg.TaskExecutionRoleArn),
		},
		{
			Name:  aws.String("TaskRoleArn"),
			Value: aws.String(runnerCfg.TaskRoleArn),
		},
		{
			Name:  aws.String("AWSAccountID"),
			Value: aws.String(runnerCfg.AWSAccountID),
		},
		{
			//Bedrock (Phase 8): mirror the ARN set by the CloudFormation runner task
			//definition so it survives auto-upgrade re-registration.
			Name:  aws.String("BedrockRoleArn"),
			Value: aws.String(runnerCfg.BedrockRoleArn),
		},
	}

	containerDefinition := ecsTypes.ContainerDefinition{
		DisableNetworking: aws.Bool(false),
		Environment:       envVars,
		Essential:         aws.Bool(true),
		Image:             aws.String(runnerCfg.DockerImage),
		Interactive:       aws.Bool(false),
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options: map[string]string{
				"awslogs-create-group":  "true",
				"awslogs-group":         logGroupName,
				"awslogs-region":        runnerCfg.Region,
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

	if runnerCfg.CpuStr == "arm" {
		cpuArch = ecsTypes.CPUArchitectureArm64
	}

	osFamily := ecsTypes.OSFamilyLinux
	if runnerCfg.OsStr == "windows" {
		osFamily = ecsTypes.OSFamilyWindowsServer2022Core
	}

	registerTaskDefinitionInput := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{
			containerDefinition,
		},
		ExecutionRoleArn: aws.String(runnerCfg.TaskExecutionRoleArn),
		Family:           aws.String(taskDefinitionFamilyName),
		Memory:           aws.String(runnerCfg.Memory),
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
		TaskRoleArn: aws.String(runnerCfg.TaskRoleArn),
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

func UpgradeDeploymentRunner(runnerCfg RunnerTaskDefinitionConfig) (string, error) {
	currentDockerImage := runnerCfg.DockerImage
	runnerUpgradeDockerImage, _, upgradeFromTs, upgradeToTs := UpgradeData.Get()
	now := time.Now().Unix()
	if now > upgradeFromTs && now < upgradeToTs {
		if len(runnerUpgradeDockerImage) > 0 && currentDockerImage != runnerUpgradeDockerImage {
			//upgrade deployment runner to upgraded image
			awsCfg, err := config.LoadDefaultConfig(context.TODO())
			if err != nil {
				return currentDockerImage, err
			}
			ecsClient := ecs.NewFromConfig(awsCfg, func(o *ecs.Options) {
				o.Region = runnerCfg.Region
			})

			//register new task definition with the upgraded image
			runnerCfg.DockerImage = runnerUpgradeDockerImage
			taskDefinitionArn, err := registerDeploymentRunnerTaskDefinition(ecsClient, runnerCfg)
			if err != nil {
				return currentDockerImage, err
			}

			//update service
			err = updateDeploymentRunnerService(ecsClient, runnerCfg.OrganizationID, runnerCfg.OsStr, runnerCfg.CpuStr, taskDefinitionArn, runnerCfg.Region)
			if err != nil {
				return currentDockerImage, err
			}
			return runnerUpgradeDockerImage, nil
		}
	}
	return currentDockerImage, nil
}
