package computer

import "time"

// Platform periods and deadlines. Each deadline bounds one wait so a stalled
// step fails and releases its capacity instead of holding it.
const (
	// UnusedComputerHibernateAfter is how long a Computer with no open Session
	// and no unsettled Command stays running before it hibernates.
	UnusedComputerHibernateAfter = 10 * time.Minute

	// ComputerPreparationTimeout bounds a new Computer's preparation, including
	// the wait for its image. Image builds attached to a waiting Computer, and
	// their successors, inherit that Computer's deadline.
	ComputerPreparationTimeout = 10 * time.Minute

	// BackgroundImageBuildTimeout bounds an image build that no Computer waits
	// on (Deployment promotion and scheduled refresh), from creation, including
	// the wait for a Worker. A Computer that joins such a build keeps its own
	// preparation deadline, so raising this above ComputerPreparationTimeout
	// makes joining Computers fail first.
	BackgroundImageBuildTimeout = 10 * time.Minute

	// BootSeedBuildTimeout bounds a boot seed build's run from its assignment to
	// a Worker.
	BootSeedBuildTimeout = 10 * time.Minute

	// ExecutionStartTimeout bounds the wait for a Computer to start executing
	// accepted work, and a wake request's receipt.
	ExecutionStartTimeout = 10 * time.Minute

	// HibernateStartTimeout bounds a hibernate request's receipt until its
	// capture starts; a capture in progress is not subject to it.
	HibernateStartTimeout = 10 * time.Minute
)
