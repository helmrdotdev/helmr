package deployment

import "fmt"

const (
	verifierExecutable    = "/proc/self/exe"
	verifierChildArgument = "__helmr-verifier"
)

func RunVerifierChild(arguments []string) (bool, error) {
	if len(arguments) < 2 || arguments[1] != verifierChildArgument {
		return false, nil
	}
	if len(arguments) != 3 {
		return true, fmt.Errorf("artifact verifier requires exactly one job argument")
	}
	job, err := parseVerifierJob(arguments[2])
	if err != nil {
		return true, err
	}
	return true, runVerifierChild(job)
}

func verifierChildArguments(job verifierJob) []string {
	return []string{verifierChildArgument, string(job)}
}
