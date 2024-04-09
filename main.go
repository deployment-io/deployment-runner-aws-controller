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
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-aws-controller/client"
	"github.com/deployment-io/deployment-runner-aws-controller/utils"
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/joho/godotenv"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
)

var clientCertPem, clientKeyPem, imageTag string

func getEnvironment() (service, organizationId, token, region, dockerImage, dockerRunnerImage, memory,
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

func getDeploymentRunnerAsgName(osStr, cpuStr, organizationID, region string) string {
	osCpuStr := fmt.Sprintf("%s%s", osStr, cpuStr)
	return fmt.Sprintf("dr-asg-%s-%s-%s", osCpuStr, organizationID, region)
}

func isDeploymentRunnerLive(region, osStr, cpuStr, organizationID string) (bool, error) {
	asgClient, err := getAsgClient(region)
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

func startOrStopDeploymentRunner(region, osStr, cpuStr, organizationId string, start bool) error {
	asgClient, err := getAsgClient(region)
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

	ecsClient, err := getEcsClient(region)
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

func main() {
	//tagInt := 0
	//if len(imageTag) > 0 {
	//	var err error
	//	tagInt, err = strconv.Atoi(imageTag)
	//	if err != nil {
	//		log.Fatal(err)
	//	}
	//}

	service, organizationId, token, region, dockerImage, runnerDockerImage, memory, taskExecutionRoleArn,
		taskRoleArn, awsAccountID := getEnvironment()
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal(err)
	}
	stsClient := sts.NewFromConfig(cfg, func(options *sts.Options) {
		options.Region = region
	})

	//aws case - check account validity
	getCallerIdentityOutput, err := stsClient.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Fatal(err)
	}
	if awsAccountID != aws.ToString(getCallerIdentityOutput.Account) {
		log.Fatalf("Invalid AWS account ID")
	}

	goarch := runtime.GOARCH
	archEnum := cpu_architecture_enums.AMD
	if strings.HasPrefix(goarch, "arm") {
		archEnum = cpu_architecture_enums.ARM
	}

	cpuStr := archEnum.String()

	goos := runtime.GOOS
	osType := os_enums.LINUX
	if strings.HasPrefix(goos, "windows") {
		osType = os_enums.WINDOWS
	}

	osStr := osType.String()

	//if tagInt > 3 {
	//check and add policy for deployment runner controller start
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsDeploymentRunnerControllerStart, osStr, cpuStr, organizationId, region)
	if err != nil {
		log.Fatal(err)
	}
	//}

	client.Connect(service, organizationId, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID,
		false)
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
			deploymentRunnerLive, err := isDeploymentRunnerLive(region, osStr, cpuStr, organizationId)
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
					err = startOrStopDeploymentRunner(region, osStr, cpuStr, organizationId, false)
					if err == nil {
						deploymentRunnerLive = false
					}
				}
			} else {
				if noPendingJobs {
					//no pending jobs - upgrade deployment runner to upgraded image
					runnerDockerImage, err = utils.UpgradeDeploymentRunner(service, organizationId, token, region, runnerDockerImage,
						osStr, cpuStr, memory, taskExecutionRoleArn, taskRoleArn, awsAccountID)
					if err != nil {
						fmt.Println(err.Error())
					}
				} else {
					//switch on
					err = startOrStopDeploymentRunner(region, osStr, cpuStr, organizationId, true)
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
