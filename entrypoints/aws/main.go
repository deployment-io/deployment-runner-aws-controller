package main

import (
	"context"
	"fmt"
	goShutdownHook "github.com/ankit-arora/go-utils/go-shutdown-hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/deployment-io/deployment-runner-aws-controller/client"
	"github.com/deployment-io/deployment-runner-aws-controller/entrypoints/common"
	"github.com/deployment-io/deployment-runner-aws-controller/utils"
	"github.com/deployment-io/deployment-runner-kit/enums/cpu_architecture_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/iam_policy_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/os_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/iam_policies"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"log"
	"runtime"
	"strings"
	"time"
)

var clientCertPem, clientKeyPem, imageTag string

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
		taskRoleArn, awsAccountID := common.GetEnvironment()
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
	err = iam_policies.AddAwsPolicyForDeploymentRunner(iam_policy_enums.AwsDeploymentRunnerControllerStart, osStr,
		cpuStr, organizationId, region, runner_enums.AwsEcs, runner_enums.AwsCloud)
	if err != nil {
		log.Fatal(err)
	}
	//}

	client.Connect(service, organizationId, token, clientCertPem, clientKeyPem, dockerImage, region, awsAccountID,
		false)
	c := client.Get()
	shutdownSignal := make(chan struct{})
	goShutdownHook.ADD(func() {
		//log.Println("waiting for shutdown signal")
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
			deploymentRunnerLive, err := common.IsDeploymentRunnerLive(region, osStr, cpuStr, organizationId)
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
					err = common.StartOrStopDeploymentRunner(region, osStr, cpuStr, organizationId, false)
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
					err = common.StartOrStopDeploymentRunner(region, osStr, cpuStr, organizationId, true)
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
	log.Println("waiting for pending jobs to complete......")
	goShutdownHook.Wait()
}
