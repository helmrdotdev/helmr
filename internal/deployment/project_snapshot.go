package deployment

import "errors"

type managerProject struct {
	descriptor ManagerArtifact
	snapshot   *artifactSnapshot
}

func (project *managerProject) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if project == nil || project.snapshot == nil {
		return errors.New("manager project is closed")
	}
	return project.snapshot.LinkInto(directory, name, uid, gid)
}

func (project *managerProject) Close() error {
	if project == nil || project.snapshot == nil {
		return nil
	}
	err := project.snapshot.Close()
	project.snapshot = nil
	return err
}
