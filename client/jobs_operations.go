package client

import (
	"github.com/deployment-io/deployment-runner-kit/jobs"
)

func (r *RunnerClient) GetPendingJobsCount() (int, int, error) {
	if !r.isConnected {
		return 0, 0, ErrConnection
	}
	args := jobs.JobsCountArgsV1{}
	args.OrganizationID = r.organizationID
	args.Token = r.token
	var jobsDto jobs.JobsCountDtoV1
	err := r.c.Call("Jobs.GetPendingCountV1", args, &jobsDto)
	if err != nil {
		return 0, 0, err
	}
	return jobsDto.Count, jobsDto.RunnerOnTimeoutSeconds, nil
}
