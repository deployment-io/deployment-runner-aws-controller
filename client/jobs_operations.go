package client

import (
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"runtime"
)

func (r *RunnerClient) GetPendingJobsCount() (int, int, error) {
	if !r.isConnected {
		return 0, 0, ErrConnection
	}
	args := jobs.JobsCountArgsV2{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	args.CloudAccountID = r.awsAccountID
	args.RunnerRegion = r.runnerRegion
	args.GoArch = runtime.GOARCH
	args.GoOS = runtime.GOOS
	var jobsDto jobs.JobsCountDtoV1
	err := r.c.Call("Jobs.GetPendingCountV2", args, &jobsDto)
	if err != nil {
		return 0, 0, err
	}
	return jobsDto.Count, jobsDto.RunnerOnTimeoutSeconds, nil
}
