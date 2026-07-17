package deployment

const (
	programVerifierExecutable    = "/proc/self/exe"
	programVerifierChildArgument = "__helmr-program-verifier"
)

func RunProgramVerifierChild(arguments []string) (bool, error) {
	if len(arguments) != 2 || arguments[1] != programVerifierChildArgument {
		return false, nil
	}
	return true, runProgramVerifierChild()
}

func programVerifierChildArguments() []string {
	return []string{programVerifierChildArgument}
}
