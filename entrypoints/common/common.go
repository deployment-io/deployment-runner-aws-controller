package common

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-kit/cloud_api_clients"
	"github.com/joho/godotenv"
	"os"
)

func GetEnvironment() (service, organizationId, token, region, dockerImage, dockerRunnerImage, memory,
	taskExecutionRoleArn, taskRoleArn, awsAccountID string) {
	//load .env ignoring err
	_ = godotenv.Load()
	organizationId = os.Getenv("OrganizationID")
	service = os.Getenv("Service")
	token = os.Getenv("Token")
	region = os.Getenv("Region")
	dockerImage = os.Getenv("DockerImage")
	dockerRunnerImage = os.Getenv("DockerRunnerImage")
	memory = os.Getenv("Memory")
	taskExecutionRoleArn = os.Getenv("ExecutionRoleArn")
	taskRoleArn = os.Getenv("TaskRoleArn")
	awsAccountID = os.Getenv("AWSAccountID")
	return
}

func getDeploymentRunnerAsgName(osStr, cpuStr, organizationID, region string) string {
	osCpuStr := fmt.Sprintf("%s%s", osStr, cpuStr)
	return fmt.Sprintf("dr-asg-%s-%s-%s", osCpuStr, organizationID, region)
}

func IsDeploymentRunnerLive(region, osStr, cpuStr, organizationID string) (bool, error) {
	asgClient, err := cloud_api_clients.GetAsgClient(region)
	if err != nil {
		return false, err
	}

	deploymentRunnerAsgName := getDeploymentRunnerAsgName(osStr, cpuStr, organizationID, region)

	describeAutoScalingGroupsOutput, err := asgClient.DescribeAutoScalingGroups(context.TODO(), &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{deploymentRunnerAsgName},
	})

	if err != nil {
		return false, err
	}

	if describeAutoScalingGroupsOutput == nil || len(describeAutoScalingGroupsOutput.AutoScalingGroups) != 1 {
		return false, nil
	}

	if describeAutoScalingGroupsOutput.AutoScalingGroups[0].DesiredCapacity == nil || aws.ToInt32(describeAutoScalingGroupsOutput.AutoScalingGroups[0].DesiredCapacity) == 0 {
		return false, nil
	}

	return true, nil

}

func StartOrStopDeploymentRunner(region, osStr, cpuStr, organizationId string, start bool) error {
	asgClient, err := cloud_api_clients.GetAsgClient(region)
	if err != nil {
		return err
	}

	deploymentRunnerAsgName := getDeploymentRunnerAsgName(osStr, cpuStr, organizationId, region)

	var desiredCapacity int32 = 1
	if !start {
		desiredCapacity = 0
	}

	_, err = asgClient.UpdateAutoScalingGroup(context.TODO(), &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(deploymentRunnerAsgName),
		DesiredCapacity:      aws.Int32(desiredCapacity),
	})

	if err != nil {
		return err
	}

	ecsClient, err := cloud_api_clients.GetEcsClientFromRegion(region)
	if err != nil {
		return err
	}

	var desiredCount int32 = 1
	if !start {
		desiredCount = 0
	}

	osCpuStr := fmt.Sprintf("%s%s", osStr, cpuStr)
	ecsServiceName := fmt.Sprintf("dr-%s-%s-%s", osCpuStr, organizationId, region)
	ecsClusterName := fmt.Sprintf("dr-%s-%s", osCpuStr, organizationId)

	updateServiceInput := &ecs.UpdateServiceInput{
		Service:       aws.String(ecsServiceName),
		Cluster:       aws.String(ecsClusterName),
		DesiredCount:  aws.Int32(desiredCount),
		PropagateTags: ecsTypes.PropagateTagsTaskDefinition,
	}
	_, err = ecsClient.UpdateService(context.TODO(), updateServiceInput)

	if err != nil {
		return err
	}

	return nil
}
