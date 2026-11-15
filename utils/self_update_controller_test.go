package utils

import (
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

const (
	testOwnImage    = "docker.io/arorankit/deployment-runner-aws-controller-amd64:5"
	testTargetImage = "docker.io/arorankit/deployment-runner-aws-controller-amd64:6"
	testRunnerImage = "docker.io/arorankit/deployment-runner-amd64:9"
	testOwnArn      = "arn:aws:ecs:us-east-1:123:task-definition/dr-aws-controller-linuxamd-org-us-east-1:3"
	testNewerArn    = "arn:aws:ecs:us-east-1:123:task-definition/dr-aws-controller-linuxamd-org-us-east-1:4"
)

func env(name, value string) ecsTypes.KeyValuePair {
	return ecsTypes.KeyValuePair{Name: aws.String(name), Value: aws.String(value)}
}

func controllerContainer(environment []ecsTypes.KeyValuePair) ecsTypes.ContainerDefinition {
	return ecsTypes.ContainerDefinition{
		Name:        aws.String("dr-aws-controller"),
		Image:       aws.String(testOwnImage),
		Essential:   aws.Bool(true),
		Environment: environment,
		LogConfiguration: &ecsTypes.LogConfiguration{
			LogDriver: ecsTypes.LogDriverAwslogs,
			Options:   map[string]string{"awslogs-group": "dr-logs-group"},
		},
	}
}

// fullTaskDefinition sets every field RegisterTaskDefinitionInput accepts, plus describe-only fields.
func fullTaskDefinition(containers ...ecsTypes.ContainerDefinition) *ecs.DescribeTaskDefinitionOutput {
	return &ecs.DescribeTaskDefinitionOutput{
		TaskDefinition: &ecsTypes.TaskDefinition{
			ContainerDefinitions:  containers,
			Family:                aws.String("dr-aws-controller-linuxamd-org-us-east-1"),
			Cpu:                   aws.String("256"),
			EphemeralStorage:      &ecsTypes.EphemeralStorage{SizeInGiB: 30},
			ExecutionRoleArn:      aws.String("arn:aws:iam::123:role/eTERole-x"),
			InferenceAccelerators: []ecsTypes.InferenceAccelerator{{DeviceName: aws.String("d"), DeviceType: aws.String("t")}},
			IpcMode:               ecsTypes.IpcModeTask,
			Memory:                aws.String("512"),
			NetworkMode:           ecsTypes.NetworkModeAwsvpc,
			PidMode:               ecsTypes.PidModeTask,
			PlacementConstraints: []ecsTypes.TaskDefinitionPlacementConstraint{{
				Type: ecsTypes.TaskDefinitionPlacementConstraintTypeMemberOf, Expression: aws.String("e")}},
			ProxyConfiguration:      &ecsTypes.ProxyConfiguration{ContainerName: aws.String("p")},
			RequiresCompatibilities: []ecsTypes.Compatibility{ecsTypes.CompatibilityFargate},
			RuntimePlatform: &ecsTypes.RuntimePlatform{CpuArchitecture: ecsTypes.CPUArchitectureX8664,
				OperatingSystemFamily: ecsTypes.OSFamilyLinux},
			TaskRoleArn: aws.String("arn:aws:iam::123:role/dr-task-role-x"),
			Volumes:     []ecsTypes.Volume{{Name: aws.String("v")}},

			Compatibilities:    []ecsTypes.Compatibility{ecsTypes.CompatibilityEc2, ecsTypes.CompatibilityFargate},
			RequiresAttributes: []ecsTypes.Attribute{{Name: aws.String("a")}},
			Revision:           3,
			Status:             ecsTypes.TaskDefinitionStatusActive,
			TaskDefinitionArn:  aws.String(testOwnArn),
			RegisteredBy:       aws.String("someone"),
		},
		Tags: []ecsTypes.Tag{
			{Key: aws.String("Name"), Value: aws.String("dr-aws-controller")},
			{Key: aws.String("created by"), Value: aws.String("deployment.io")},
		},
	}
}

func TestBuildControllerTaskDefinitionInputKeepsEverythingElse(t *testing.T) {
	originalEnv := []ecsTypes.KeyValuePair{
		env("Token", "secret"),
		env("DockerImage", testOwnImage),
		env("BedrockRoleArn", "arn:aws:iam::123:role/bedrock"),
		env("DockerRunnerImage", "docker.io/arorankit/deployment-runner-amd64:8"),
		env("Region", "us-east-1"),
	}
	described := fullTaskDefinition(controllerContainer(originalEnv))

	input, err := buildControllerTaskDefinitionInput(described, testTargetImage, testRunnerImage)
	if err != nil {
		t.Fatal(err)
	}

	// Every field of the input must be set when every source field is set.
	inputValue := reflect.ValueOf(*input)
	for i := 0; i < inputValue.NumField(); i++ {
		if inputValue.Type().Field(i).IsExported() && inputValue.Field(i).IsZero() {
			t.Errorf("field %s not copied", inputValue.Type().Field(i).Name)
		}
	}

	td := described.TaskDefinition
	expected := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []ecsTypes.ContainerDefinition{func() ecsTypes.ContainerDefinition {
			c := controllerContainer([]ecsTypes.KeyValuePair{
				env("Token", "secret"),
				env("DockerImage", testTargetImage),
				env("BedrockRoleArn", "arn:aws:iam::123:role/bedrock"),
				env("DockerRunnerImage", testRunnerImage),
				env("Region", "us-east-1"),
			})
			c.Image = aws.String(testTargetImage)
			return c
		}()},
		Family:                  td.Family,
		Cpu:                     td.Cpu,
		EphemeralStorage:        td.EphemeralStorage,
		ExecutionRoleArn:        td.ExecutionRoleArn,
		InferenceAccelerators:   td.InferenceAccelerators,
		IpcMode:                 td.IpcMode,
		Memory:                  td.Memory,
		NetworkMode:             td.NetworkMode,
		PidMode:                 td.PidMode,
		PlacementConstraints:    td.PlacementConstraints,
		ProxyConfiguration:      td.ProxyConfiguration,
		RequiresCompatibilities: td.RequiresCompatibilities,
		RuntimePlatform:         td.RuntimePlatform,
		Tags:                    described.Tags,
		TaskRoleArn:             td.TaskRoleArn,
		Volumes:                 td.Volumes,
	}
	if !reflect.DeepEqual(input, expected) {
		t.Errorf("unexpected input:\n got %+v\nwant %+v", input, expected)
	}

	// The described definition is left unchanged.
	if aws.ToString(td.ContainerDefinitions[0].Image) != testOwnImage ||
		aws.ToString(td.ContainerDefinitions[0].Environment[1].Value) != testOwnImage {
		t.Error("described task definition was modified")
	}
}

func TestBuildControllerTaskDefinitionInputEmptyStaysEmpty(t *testing.T) {
	described := fullTaskDefinition(controllerContainer(nil))
	described.TaskDefinition.RequiresCompatibilities = nil
	input, err := buildControllerTaskDefinitionInput(described, testTargetImage, testRunnerImage)
	if err != nil {
		t.Fatal(err)
	}
	if input.RequiresCompatibilities != nil {
		t.Errorf("RequiresCompatibilities filled in: %v", input.RequiresCompatibilities)
	}
}

func TestBuildControllerTaskDefinitionInputAddsMissingEntries(t *testing.T) {
	described := fullTaskDefinition(controllerContainer([]ecsTypes.KeyValuePair{env("Token", "secret")}))
	input, err := buildControllerTaskDefinitionInput(described, testTargetImage, testRunnerImage)
	if err != nil {
		t.Fatal(err)
	}
	expected := []ecsTypes.KeyValuePair{
		env("Token", "secret"),
		env("DockerImage", testTargetImage),
		env("DockerRunnerImage", testRunnerImage),
	}
	if got := input.ContainerDefinitions[0].Environment; !reflect.DeepEqual(got, expected) {
		t.Errorf("environment: got %+v, want %+v", got, expected)
	}
	if len(described.TaskDefinition.ContainerDefinitions[0].Environment) != 1 {
		t.Error("described environment was modified")
	}
}

func TestBuildControllerTaskDefinitionInputRefusesTwoContainers(t *testing.T) {
	described := fullTaskDefinition(controllerContainer(nil), controllerContainer(nil))
	if _, err := buildControllerTaskDefinitionInput(described, testTargetImage, testRunnerImage); err == nil {
		t.Error("expected an error for two containers")
	}
}

func TestDecideSelfUpdate(t *testing.T) {
	inWindow := SelfUpdateDecisionInput{
		Now: 1000, UpgradeFromTs: 900, UpgradeToTs: 1100,
		TargetImage: testTargetImage, OwnImage: testOwnImage,
		NewestRevisionArn: testOwnArn, OwnRevisionArn: testOwnArn,
	}
	tests := []struct {
		name   string
		change func(in *SelfUpdateDecisionInput)
		want   selfUpdateDecision
	}{
		{"inside the window with a new target", func(in *SelfUpdateDecisionInput) {}, selfUpdateAttempt},
		{"before the window", func(in *SelfUpdateDecisionInput) { in.Now = 900 }, selfUpdateNone},
		{"after the window", func(in *SelfUpdateDecisionInput) { in.Now = 1100 }, selfUpdateNone},
		{"empty target", func(in *SelfUpdateDecisionInput) { in.TargetImage = "" }, selfUpdateNone},
		{"target is own image", func(in *SelfUpdateDecisionInput) { in.TargetImage = testOwnImage }, selfUpdateNone},
		{"newest revision at target, not running: rolled back", func(in *SelfUpdateDecisionInput) {
			in.NewestRevisionArn, in.NewestRevisionImage = testNewerArn, testTargetImage
		}, selfUpdateRolledBack},
		{"newest revision at target registered by this process", func(in *SelfUpdateDecisionInput) {
			in.NewestRevisionArn, in.NewestRevisionImage = testNewerArn, testTargetImage
			in.NewestRegisteredHere = true
		}, selfUpdateAttempt},
		{"newest revision at another image", func(in *SelfUpdateDecisionInput) {
			in.NewestRevisionArn, in.NewestRevisionImage = testNewerArn, "other:1"
		}, selfUpdateAttempt},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := inWindow
			test.change(&in)
			if got := decideSelfUpdate(in); got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestSelfUpdateThrottle(t *testing.T) {
	var throttle selfUpdateThrottle
	start := time.Unix(1_000_000, 0)
	if !throttle.allow("a", start) {
		t.Fatal("first attempt not allowed")
	}
	if throttle.allow("a", start.Add(2*time.Second)) {
		t.Error("second attempt within 10 minutes allowed")
	}
	if throttle.allow("a", start.Add(selfUpdateRetryInterval-time.Second)) {
		t.Error("attempt just under 10 minutes allowed")
	}
	if !throttle.allow("a", start.Add(selfUpdateRetryInterval)) {
		t.Error("attempt after 10 minutes not allowed")
	}
	if !throttle.allow("b", start.Add(selfUpdateRetryInterval+time.Second)) {
		t.Error("attempt to a new target image not allowed")
	}
	// a's attempt at start+10m keeps its cooldown although b was attempted since.
	if throttle.allow("a", start.Add(selfUpdateRetryInterval+2*time.Second)) {
		t.Error("attempt to a within 10 minutes allowed after switching to b")
	}
	if !throttle.allow("a", start.Add(2*selfUpdateRetryInterval)) {
		t.Error("attempt to a after 10 minutes not allowed")
	}
}

func TestParseTaskMetadata(t *testing.T) {
	body := []byte(`{"Cluster":"arn:aws:ecs:us-east-1:123:cluster/dr-linuxamd-org",
		"TaskARN":"arn:aws:ecs:us-east-1:123:task/dr-linuxamd-org/abc","Family":"f","Revision":"3",
		"Containers":[{"Name":"c"}]}`)
	metadata, err := parseTaskMetadata(body)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Cluster != "arn:aws:ecs:us-east-1:123:cluster/dr-linuxamd-org" ||
		metadata.TaskARN != "arn:aws:ecs:us-east-1:123:task/dr-linuxamd-org/abc" {
		t.Errorf("unexpected metadata %+v", metadata)
	}
	for _, bad := range []string{`not json`, `{}`, `{"Cluster":"c"}`, `{"TaskARN":"t"}`} {
		if _, err := parseTaskMetadata([]byte(bad)); err == nil {
			t.Errorf("expected an error for %s", bad)
		}
	}
}

func TestServiceNameFromGroup(t *testing.T) {
	if name, ok := serviceNameFromGroup("service:x"); !ok || name != "x" {
		t.Errorf("service:x → %q, %v", name, ok)
	}
	for _, group := range []string{"", "family:x", "service:", "x"} {
		if name, ok := serviceNameFromGroup(group); ok {
			t.Errorf("%q → %q, want none", group, name)
		}
	}
}

func TestTaskDefinitionFamilyOf(t *testing.T) {
	if got := taskDefinitionFamilyOf(testOwnArn); got != "dr-aws-controller-linuxamd-org-us-east-1" {
		t.Errorf("got %q", got)
	}
}
