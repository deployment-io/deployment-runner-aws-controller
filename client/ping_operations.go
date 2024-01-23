package client

import (
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/ping"
	"runtime"
)

func (r *RunnerClient) Ping(firstPing bool) (string, string, int64, int64, error) {
	args := ping.ArgsV2{}
	args.Send = "ping"
	args.FirstPing = firstPing
	args.GoArch = runtime.GOARCH
	args.GoOS = runtime.GOOS
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.DockerImage = r.currentDockerImage
	args.CloudAccountID = r.awsAccountID
	args.RunnerRegion = r.runnerRegion
	var reply ping.ReplyV2
	err := r.c.Call("Ping.SendFromControllerV2", args, &reply)
	if err != nil {
		return "", "", 0, 0, err
	}
	if reply.Send != "pong" {
		return "", "", 0, 0, fmt.Errorf("error receiving pong from the server")
	}

	return reply.RunnerUpgradeDockerImage, reply.ControllerUpgradeDockerImage, reply.UpgradeFromTs, reply.UpgradeToTs, nil
}
