package guestd

import (
	"context"
	"errors"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

// runScopedCommand joins the main process, kills remaining descendants and
// drains their output before returning. Only that complete success permits a cut.
func (entry *computerMountEntry) executePreparation(ctx context.Context, start *computerv0.PreparationStart, output *preparationOutput) (error, error) {
	secrets, err := computerBasicExecSecrets(start.GetSecrets())
	if err != nil {
		return err, nil
	}
	defer clearProgramSecretValues(secrets)
	cwd, err := entry.computerLaunchCwd("")
	if err != nil {
		return err, nil
	}
	env := managedRuntimeEnv(entry.imageConfig, entry.runtimeUser, cwd)
	if err := stageProtectedEnv(entry.imageRoot, start.GetProtectedEnv(), start.GetProxyCa(), &env); err != nil {
		return err, nil
	}
	secretRoot, cleanupSecrets, err := stageProgramSecrets(entry.imageRoot, secrets, entry.runtimeUser, &env)
	if err != nil {
		return err, nil
	}
	defer cleanupSecrets()
	if err := prepareLaunchPath(entry.imageRoot, cwd, entry.runtimeUser); err != nil {
		return err, nil
	}
	cleanupRuntime, err := mountImageRuntimeFilesystems(entry.imageRoot)
	if err != nil {
		return err, nil
	}
	defer cleanupRuntime()
	program := bootProgramMounts()
	flags, err := managedProgramNodeFlags(program.Runtime)
	if err != nil {
		return err, nil
	}
	leaf, err := execCgroupLeafName("preparation:" + entry.computerID)
	if err != nil {
		return err, nil
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := imageCommand(lifetime, managedProgramNode, append(flags, managedProgramEntry, "prepare", start.GetComputerDefinitionId()), cwd, env, entry.imageRoot, entry.runtimeUser, imageCommandOptions{Program: program, SecretRoot: secretRoot, CgroupNamespace: true, CgroupLeaf: leaf})
	if err != nil {
		return err, nil
	}
	cmd.Stdout = preparationOutputWriter{buffer: output.stream("stdout")}
	cmd.Stderr = preparationOutputWriter{buffer: output.stream("stderr")}
	scope, err := createProcessCgroup(leaf)
	if err != nil {
		return err, nil
	}
	runErr, cleanupErr := runScopedCommand(cmd, scope, output.pipeClosed)
	runErr = errors.Join(runErr, ctx.Err())
	return runErr, cleanupErr
}
