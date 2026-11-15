package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// selfUpdateRetryInterval is the minimum time between two self-update attempts to the same image.
// The idle loop runs every 2 seconds, so without it a failing attempt would register a task
// definition each time round.
const selfUpdateRetryInterval = 10 * time.Minute

const (
	controllerDockerImageEnvName       = "DockerImage"
	controllerDockerRunnerImageEnvName = "DockerRunnerImage"
	ecsMetadataURIEnvName              = "ECS_CONTAINER_METADATA_URI_V4"
	serviceGroupPrefix                 = "service:"
)

// errSelfUpdateUnsupported marks a condition that won't change in this process (no task metadata,
// a task not started by a service, a definition with more than one container).
var errSelfUpdateUnsupported = errors.New("self-update not supported")

// taskMetadata is the subset of the ECS task metadata endpoint v4 <uri>/task response we read.
type taskMetadata struct {
	Cluster string `json:"Cluster"`
	TaskARN string `json:"TaskARN"`
}

func parseTaskMetadata(body []byte) (taskMetadata, error) {
	var metadata taskMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return taskMetadata{}, fmt.Errorf("parsing task metadata: %w", err)
	}
	if len(metadata.Cluster) == 0 || len(metadata.TaskARN) == 0 {
		return taskMetadata{}, fmt.Errorf("task metadata has no Cluster or TaskARN")
	}
	return metadata, nil
}

// serviceNameFromGroup returns the service name of a task a service started (group "service:<name>").
func serviceNameFromGroup(group string) (string, bool) {
	if !strings.HasPrefix(group, serviceGroupPrefix) {
		return "", false
	}
	serviceName := strings.TrimPrefix(group, serviceGroupPrefix)
	if len(serviceName) == 0 {
		return "", false
	}
	return serviceName, true
}

// setEnvironmentValue sets name to value in env, appending the entry if it isn't there.
// env must not share its backing array with a slice the caller wants unchanged.
func setEnvironmentValue(env []ecsTypes.KeyValuePair, name, value string) []ecsTypes.KeyValuePair {
	found := false
	for i := range env {
		if aws.ToString(env[i].Name) == name {
			env[i].Value = aws.String(value)
			found = true
		}
	}
	if !found {
		env = append(env, ecsTypes.KeyValuePair{Name: aws.String(name), Value: aws.String(value)})
	}
	return env
}

// buildControllerTaskDefinitionInput copies the described controller task definition into a
// RegisterTaskDefinitionInput, changing only the container's Image and its DockerImage and
// DockerRunnerImage environment entries.
func buildControllerTaskDefinitionInput(described *ecs.DescribeTaskDefinitionOutput, targetImage,
	runnerImage string) (*ecs.RegisterTaskDefinitionInput, error) {
	if described == nil || described.TaskDefinition == nil {
		return nil, fmt.Errorf("no task definition described")
	}
	taskDefinition := described.TaskDefinition
	if len(taskDefinition.ContainerDefinitions) != 1 {
		return nil, fmt.Errorf("%w: task definition %s has %d containers, expected 1", errSelfUpdateUnsupported,
			aws.ToString(taskDefinition.TaskDefinitionArn), len(taskDefinition.ContainerDefinitions))
	}

	container := taskDefinition.ContainerDefinitions[0]
	container.Image = aws.String(targetImage)
	environment := append([]ecsTypes.KeyValuePair(nil), container.Environment...)
	environment = setEnvironmentValue(environment, controllerDockerImageEnvName, targetImage)
	environment = setEnvironmentValue(environment, controllerDockerRunnerImageEnvName, runnerImage)
	container.Environment = environment

	return &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:    []ecsTypes.ContainerDefinition{container},
		Family:                  taskDefinition.Family,
		Cpu:                     taskDefinition.Cpu,
		EphemeralStorage:        taskDefinition.EphemeralStorage,
		ExecutionRoleArn:        taskDefinition.ExecutionRoleArn,
		InferenceAccelerators:   taskDefinition.InferenceAccelerators,
		IpcMode:                 taskDefinition.IpcMode,
		Memory:                  taskDefinition.Memory,
		NetworkMode:             taskDefinition.NetworkMode,
		PidMode:                 taskDefinition.PidMode,
		PlacementConstraints:    taskDefinition.PlacementConstraints,
		ProxyConfiguration:      taskDefinition.ProxyConfiguration,
		RequiresCompatibilities: taskDefinition.RequiresCompatibilities,
		RuntimePlatform:         taskDefinition.RuntimePlatform,
		Tags:                    described.Tags,
		TaskRoleArn:             taskDefinition.TaskRoleArn,
		Volumes:                 taskDefinition.Volumes,
	}, nil
}

// SelfUpdateDecisionInput groups what decideSelfUpdate needs (four-parameter limit).
type SelfUpdateDecisionInput struct {
	Now           int64
	UpgradeFromTs int64
	UpgradeToTs   int64
	TargetImage   string
	OwnImage      string
	// NewestRevisionArn and NewestRevisionImage describe the newest revision of the controller's
	// task definition family; OwnRevisionArn is the revision this task runs.
	NewestRevisionArn   string
	NewestRevisionImage string
	OwnRevisionArn      string
	// NewestRegisteredHere is true when this process registered the newest revision itself (an
	// earlier attempt whose UpdateService failed), so it isn't a rolled-back revision.
	NewestRegisteredHere bool
}

type selfUpdateDecision int

const (
	selfUpdateNone selfUpdateDecision = iota
	selfUpdateAttempt
	selfUpdateRolledBack
)

// selfUpdateWanted reports whether deployment-server names a controller image other than ours
// inside the upgrade window.
func selfUpdateWanted(in SelfUpdateDecisionInput) bool {
	if len(in.TargetImage) == 0 || in.TargetImage == in.OwnImage {
		return false
	}
	return in.Now > in.UpgradeFromTs && in.Now < in.UpgradeToTs
}

func decideSelfUpdate(in SelfUpdateDecisionInput) selfUpdateDecision {
	if !selfUpdateWanted(in) {
		return selfUpdateNone
	}
	if in.NewestRevisionImage == in.TargetImage && in.NewestRevisionArn != in.OwnRevisionArn &&
		!in.NewestRegisteredHere {
		return selfUpdateRolledBack
	}
	return selfUpdateAttempt
}

// selfUpdateThrottle allows at most one attempt per target image every selfUpdateRetryInterval.
// Each target image keeps its own cooldown, so switching targets doesn't reset another's.
type selfUpdateThrottle struct {
	attemptAt map[string]time.Time
}

func (t *selfUpdateThrottle) allow(image string, now time.Time) bool {
	if last, ok := t.attemptAt[image]; ok && now.Sub(last) < selfUpdateRetryInterval {
		return false
	}
	if t.attemptAt == nil {
		t.attemptAt = map[string]time.Time{}
	}
	t.attemptAt[image] = now
	return true
}

// registeredRevision is a revision this process registered for a target image, and the runner
// image it put in DockerRunnerImage.
type registeredRevision struct {
	arn         string
	runnerImage string
}

type controllerIdentity struct {
	cluster           string
	serviceName       string
	taskDefinitionArn string
}

// ControllerSelfUpdater moves the controller's own ECS service to the controller image
// deployment-server names, the way UpgradeDeploymentRunner moves the runner.
type ControllerSelfUpdater struct {
	region   string
	ownImage string

	ecsClient  *ecs.Client
	identity   *controllerIdentity
	throttle   selfUpdateThrottle
	disabled   bool
	done       bool
	rolledBack map[string]bool
	// registered maps a target image to the revision this process last registered for it.
	registered map[string]registeredRevision
}

func NewControllerSelfUpdater(region, ownImage string) *ControllerSelfUpdater {
	return &ControllerSelfUpdater{
		region:     region,
		ownImage:   ownImage,
		rolledBack: map[string]bool{},
		registered: map[string]registeredRevision{},
	}
}

// MaybeSelfUpdate attempts a self-update when one is due. runnerImage is the runner image the
// controller currently uses. Failures are logged, never fatal.
func (u *ControllerSelfUpdater) MaybeSelfUpdate(runnerImage string) {
	if u.disabled || u.done {
		return
	}
	_, targetImage, upgradeFromTs, upgradeToTs := UpgradeData.Get()
	in := SelfUpdateDecisionInput{
		Now:           time.Now().Unix(),
		UpgradeFromTs: upgradeFromTs,
		UpgradeToTs:   upgradeToTs,
		TargetImage:   targetImage,
		OwnImage:      u.ownImage,
	}
	if !selfUpdateWanted(in) || u.rolledBack[targetImage] || !u.throttle.allow(targetImage, time.Now()) {
		return
	}
	err := u.attempt(in, runnerImage)
	if errors.Is(err, errSelfUpdateUnsupported) {
		log.Printf("Self-update disabled in this process: %s", err)
		u.disabled = true
	} else if err != nil {
		log.Printf("Self-update to %s failed (retrying in %s): %s", targetImage, selfUpdateRetryInterval, err)
	}
}

func (u *ControllerSelfUpdater) attempt(in SelfUpdateDecisionInput, runnerImage string) error {
	if u.ecsClient == nil {
		awsCfg, err := config.LoadDefaultConfig(context.TODO())
		if err != nil {
			return err
		}
		u.ecsClient = ecs.NewFromConfig(awsCfg, func(o *ecs.Options) {
			o.Region = u.region
		})
	}
	if u.identity == nil {
		identity, err := u.findSelf()
		if err != nil {
			return err
		}
		u.identity = identity
	}

	ownDefinition, err := u.ecsClient.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(u.identity.taskDefinitionArn),
		Include:        []ecsTypes.TaskDefinitionField{ecsTypes.TaskDefinitionFieldTags},
	})
	if err != nil {
		return fmt.Errorf("describing own task definition: %w", err)
	}
	if ownDefinition.TaskDefinition == nil {
		return fmt.Errorf("own task definition %s not found", u.identity.taskDefinitionArn)
	}
	ownRevisionArn := aws.ToString(ownDefinition.TaskDefinition.TaskDefinitionArn)

	newestArn, err := u.newestRevisionArn(aws.ToString(ownDefinition.TaskDefinition.Family))
	if err != nil {
		return err
	}
	newestImage := ""
	if len(newestArn) > 0 && newestArn != ownRevisionArn {
		newestImage, err = u.controllerImageOf(newestArn)
		if err != nil {
			return err
		}
	}
	in.OwnRevisionArn = ownRevisionArn
	in.NewestRevisionArn = newestArn
	in.NewestRevisionImage = newestImage
	registered, registeredHere := u.registered[in.TargetImage]
	in.NewestRegisteredHere = registeredHere && len(newestArn) > 0 && registered.arn == newestArn

	switch decideSelfUpdate(in) {
	case selfUpdateNone:
		return nil
	case selfUpdateRolledBack:
		log.Printf("Self-update to %s was rolled back; not retrying until deployment-server names another image",
			in.TargetImage)
		u.rolledBack[in.TargetImage] = true
		return nil
	}

	// Reuse a revision this process registered (an earlier attempt whose UpdateService failed) only
	// if it carries the runner image the controller uses now.
	newDefinitionArn := registered.arn
	if !in.NewestRegisteredHere || registered.runnerImage != runnerImage {
		registerInput, err := buildControllerTaskDefinitionInput(ownDefinition, in.TargetImage, runnerImage)
		if err != nil {
			return err
		}
		registerOutput, err := u.ecsClient.RegisterTaskDefinition(context.TODO(), registerInput)
		if err != nil {
			return fmt.Errorf("registering task definition: %w", err)
		}
		newDefinitionArn = aws.ToString(registerOutput.TaskDefinition.TaskDefinitionArn)
		u.registered[in.TargetImage] = registeredRevision{arn: newDefinitionArn, runnerImage: runnerImage}
	}

	if err = u.updateOwnService(in, newDefinitionArn); err != nil {
		return err
	}
	u.done = true
	return nil
}

// findSelf reads this task's cluster, task definition and service from the task metadata
// endpoint and ecs.DescribeTasks.
func (u *ControllerSelfUpdater) findSelf() (*controllerIdentity, error) {
	metadataURI := os.Getenv(ecsMetadataURIEnvName)
	if len(metadataURI) == 0 {
		return nil, fmt.Errorf("%w: %s is not set", errSelfUpdateUnsupported, ecsMetadataURIEnvName)
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	response, err := httpClient.Get(metadataURI + "/task")
	if err != nil {
		return nil, fmt.Errorf("reading task metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading task metadata: status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("reading task metadata: %w", err)
	}
	metadata, err := parseTaskMetadata(body)
	if err != nil {
		return nil, err
	}

	describeTasksOutput, err := u.ecsClient.DescribeTasks(context.TODO(), &ecs.DescribeTasksInput{
		Cluster: aws.String(metadata.Cluster),
		Tasks:   []string{metadata.TaskARN},
	})
	if err != nil {
		return nil, fmt.Errorf("describing own task: %w", err)
	}
	if len(describeTasksOutput.Tasks) != 1 {
		return nil, fmt.Errorf("own task %s not found", metadata.TaskARN)
	}
	task := describeTasksOutput.Tasks[0]
	serviceName, ok := serviceNameFromGroup(aws.ToString(task.Group))
	if !ok {
		return nil, fmt.Errorf("%w: task group %q is not a service", errSelfUpdateUnsupported, aws.ToString(task.Group))
	}
	return &controllerIdentity{
		cluster:           metadata.Cluster,
		serviceName:       serviceName,
		taskDefinitionArn: aws.ToString(task.TaskDefinitionArn),
	}, nil
}

// newestRevisionArn returns the newest revision of family. FamilyPrefix also matches longer
// family names, so only an exact family match counts.
func (u *ControllerSelfUpdater) newestRevisionArn(family string) (string, error) {
	paginator := ecs.NewListTaskDefinitionsPaginator(u.ecsClient, &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(family),
		Sort:         ecsTypes.SortOrderDesc,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			return "", fmt.Errorf("listing task definitions: %w", err)
		}
		for _, arn := range page.TaskDefinitionArns {
			if taskDefinitionFamilyOf(arn) == family {
				return arn, nil
			}
		}
	}
	return "", nil
}

// taskDefinitionFamilyOf returns the family of arn:aws:ecs:<region>:<account>:task-definition/<family>:<revision>.
func taskDefinitionFamilyOf(arn string) string {
	familyRevision := arn[strings.LastIndex(arn, "/")+1:]
	if i := strings.LastIndex(familyRevision, ":"); i >= 0 {
		return familyRevision[:i]
	}
	return familyRevision
}

func (u *ControllerSelfUpdater) controllerImageOf(taskDefinitionArn string) (string, error) {
	output, err := u.ecsClient.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefinitionArn),
	})
	if err != nil {
		return "", fmt.Errorf("describing task definition %s: %w", taskDefinitionArn, err)
	}
	if output.TaskDefinition == nil || len(output.TaskDefinition.ContainerDefinitions) != 1 {
		return "", nil
	}
	return aws.ToString(output.TaskDefinition.ContainerDefinitions[0].Image), nil
}

// updateOwnService points the controller's service at taskDefinitionArn, keeping its deployment
// percentages and alarms and enabling the circuit breaker with rollback.
func (u *ControllerSelfUpdater) updateOwnService(in SelfUpdateDecisionInput, taskDefinitionArn string) error {
	describeServicesOutput, err := u.ecsClient.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Cluster:  aws.String(u.identity.cluster),
		Services: []string{u.identity.serviceName},
	})
	if err != nil {
		return fmt.Errorf("describing own service: %w", err)
	}
	if len(describeServicesOutput.Services) != 1 {
		return fmt.Errorf("own service %s not found", u.identity.serviceName)
	}
	deploymentConfiguration := &ecsTypes.DeploymentConfiguration{
		DeploymentCircuitBreaker: &ecsTypes.DeploymentCircuitBreaker{Enable: true, Rollback: true},
	}
	if current := describeServicesOutput.Services[0].DeploymentConfiguration; current != nil {
		deploymentConfiguration.MaximumPercent = current.MaximumPercent
		deploymentConfiguration.MinimumHealthyPercent = current.MinimumHealthyPercent
		deploymentConfiguration.Alarms = current.Alarms
	}

	log.Printf("Self-update: %s → %s (task definition %s)", in.OwnImage, in.TargetImage, taskDefinitionArn)
	_, err = u.ecsClient.UpdateService(context.TODO(), &ecs.UpdateServiceInput{
		Cluster:                 aws.String(u.identity.cluster),
		Service:                 aws.String(u.identity.serviceName),
		TaskDefinition:          aws.String(taskDefinitionArn),
		DeploymentConfiguration: deploymentConfiguration,
	})
	if err != nil {
		return fmt.Errorf("updating own service: %w", err)
	}
	return nil
}
