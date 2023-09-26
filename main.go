package main

import (
	"context"
	"fmt"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/deployment-io/deployment-runner-aws-controller/client"
	"github.com/deployment-io/deployment-runner-aws-controller/utils"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/joho/godotenv"
	"os"
	"time"
)

var clientCertPem, clientKeyPem string

func getEnvironment() (service, organizationId, token, region, dockerImage, dockerRunnerImage, cpuStr, memory, taskExecutionRoleArn, taskRoleArn string) {
	//load .env ignoring err
	_ = godotenv.Load()
	organizationId = os.Getenv("OrganizationID")
	service = os.Getenv("Service")
	token = os.Getenv("Token")
	region = os.Getenv("Region")
	dockerImage = os.Getenv("DockerImage")
	dockerRunnerImage = os.Getenv("DockerRunnerImage")
	cpuStr = os.Getenv("CpuArch")
	memory = os.Getenv("Memory")
	taskExecutionRoleArn = os.Getenv("ExecutionRoleArn")
	taskRoleArn = os.Getenv("TaskRoleArn")

	return
}

func getAsgClient(region string) (*autoscaling.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	asgClient := autoscaling.NewFromConfig(cfg, func(options *autoscaling.Options) {
		options.Region = region
	})

	return asgClient, nil
}

func getEcsClient(region string) (*ecs.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	ecsClient := ecs.NewFromConfig(cfg, func(o *ecs.Options) {
		o.Region = region
	})
	return ecsClient, nil
}

func getDeploymentRunnerAsgName(cpuStr string) string {
	return fmt.Sprintf("deployment-runner-asg-%s", cpuStr)
}

func isDeploymentRunnerLive(region, cpuStr string) (bool, error) {
	asgClient, err := getAsgClient(region)
	if err != nil {
		return false, err
	}

	deploymentRunnerAsgName := getDeploymentRunnerAsgName(cpuStr)

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

func startOrStopDeploymentRunner(region, cpuStr, organizationId string, start bool) error {
	asgClient, err := getAsgClient(region)
	if err != nil {
		return err
	}

	deploymentRunnerAsgName := getDeploymentRunnerAsgName(cpuStr)

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

	ecsClient, err := getEcsClient(region)
	if err != nil {
		return err
	}

	var desiredCount int32 = 1
	if !start {
		desiredCount = 0
	}

	ecsServiceName := fmt.Sprintf("deployment-runner-%s", cpuStr)
	ecsClusterName := fmt.Sprintf("deployment-runner-%s-%s", cpuStr, organizationId)

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

func main() {
	service, organizationId, token, region, dockerImage, dockerRunnerImage, cpuStr, memory, taskExecutionRoleArn, taskRoleArn := getEnvironment()
	client.Connect(service, organizationId, token, clientCertPem, clientKeyPem, dockerImage, false)
	c := client.Get()
	shutdownSignal := make(chan struct{})
	goShutdownHook.ADD(func() {
		fmt.Println("waiting for shutdown signal")
		shutdownSignal <- struct{}{}
		close(shutdownSignal)
	})
	shutdown := false
	jobs.RegisterGobDataTypes()

	for !shutdown {
		select {
		case <-shutdownSignal:
			shutdown = true
		default:
			deploymentRunnerLive, err := isDeploymentRunnerLive(region, cpuStr)
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}
			//check jobs count
			pendingJobsCount, runnerOnTimeoutSeconds, err := c.GetPendingJobsCount()
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}
			noPendingJobs := pendingJobsCount == 0
			if deploymentRunnerLive {
				if noPendingJobs {
					//switch off
					err = startOrStopDeploymentRunner(region, cpuStr, organizationId, false)
					if err == nil {
						deploymentRunnerLive = false
					}
				}
			} else {
				if noPendingJobs {
					//no pending jobs - upgrade deployment runner to upgraded image
					dockerRunnerImage, err = utils.UpgradeDeploymentRunner(service, organizationId, token, region, dockerRunnerImage,
						cpuStr, memory, taskExecutionRoleArn, taskRoleArn)
					if err != nil {
						fmt.Println(err.Error())
					}
				} else {
					//switch on
					err = startOrStopDeploymentRunner(region, cpuStr, organizationId, true)
					if err == nil {
						deploymentRunnerLive = true
					}
				}
			}
			timeout := 2 * time.Second
			if deploymentRunnerLive {
				timeout = time.Duration(runnerOnTimeoutSeconds) * time.Second
			}
			time.Sleep(timeout)
		}
	}
	fmt.Println("waiting for shutdown.wait")
	goShutdownHook.Wait()
}
