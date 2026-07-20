package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const managerRunCloseTimeout = 30 * time.Second

type ManagerRunDrives struct {
	Manager           vm.ReadOnlyDriveSource
	Runtime           vm.ReadOnlyDriveSource
	StandardToolchain vm.ReadOnlyDriveSource
	Project           vm.ReadOnlyDriveSource
	OfflineStore      vm.ReadOnlyDriveSource
}

type ManagerRunner struct {
	Connector  vm.Connector
	Toolchains *ToolchainCatalog
	TempDir    string
	random     io.Reader
}

type managerRunDrive struct {
	id     string
	label  string
	source vm.ReadOnlyDriveSource
}

func (runner ManagerRunner) Run(
	ctx context.Context,
	request ManagerRequest,
	drives ManagerRunDrives,
	graph *PackageGraph,
) (
	metadata ManagerMetadata,
	tree ManagerTreeContent,
	returnErr error,
) {
	if ctx == nil {
		return ManagerMetadata{}, nil, errors.New("manager run context is nil")
	}
	if runner.Connector == nil {
		return ManagerMetadata{}, nil, errors.New("manager run connector is required")
	}
	authorization, err := AuthorizeManagerRequest(request, runner.Toolchains)
	if err != nil {
		return ManagerMetadata{}, nil, err
	}
	if request.Operation != ManagerProbe && runner.TempDir == "" {
		return ManagerMetadata{}, nil, errors.New("manager run temp directory is required")
	}
	readOnlyDrives, err := managerRunDrives(request, drives)
	if err != nil {
		return ManagerMetadata{}, nil, err
	}

	connectRequest := vm.ConnectRequest{
		OwnerKind:   vm.OwnerBuild,
		Resources:   compute.BuildGuestResources(),
		PIDsMax:     compute.DependencyGuestPIDsMax,
		Networkless: true,
		BuildDrives: readOnlyDrives,
	}
	session, closeSession, err := runner.openManagerGuest(
		ctx,
		request.Operation,
		connectRequest,
	)
	if err != nil {
		return ManagerMetadata{}, nil, vm.NewGuestError(
			fmt.Errorf("connect dependency manager guest: %w", err),
		)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(
			context.Background(),
			managerRunCloseTimeout,
		)
		defer cancel()
		if err := closeSession(closeCtx); err != nil {
			if tree != nil {
				err = errors.Join(err, tree.Close())
				tree = nil
			}
			returnErr = errors.Join(
				returnErr,
				vm.NewGuestError(fmt.Errorf("close dependency manager guest: %w", err)),
			)
		}
	}()

	stream := session.Stream()
	if stream == nil {
		return ManagerMetadata{}, nil, vm.NewGuestError(
			errors.New("dependency manager guest stream is nil"),
		)
	}
	stopClose := closeOnCancellation(ctx, stream)
	defer stopClose()
	if err := WriteManagerRequest(stream, authorization); err != nil {
		return ManagerMetadata{}, nil, vm.NewGuestError(
			fmt.Errorf(
				"write dependency manager request: %w",
				preferContextError(ctx, err),
			),
		)
	}
	if err := stream.CloseWrite(); err != nil {
		return ManagerMetadata{}, nil, vm.NewGuestError(
			fmt.Errorf(
				"half-close dependency manager request: %w",
				preferContextError(ctx, err),
			),
		)
	}
	metadata, tree, err = ReadManagerResponse(
		ctx,
		stream,
		runner.TempDir,
		request,
		graph,
	)
	if err != nil {
		return ManagerMetadata{}, nil, vm.NewGuestError(
			fmt.Errorf("read dependency manager response: %w", err),
		)
	}
	if !stopClose() {
		cause := preferContextError(ctx, io.ErrClosedPipe)
		if tree != nil {
			cause = errors.Join(cause, tree.Close())
		}
		return ManagerMetadata{}, nil, vm.NewGuestError(
			cause,
		)
	}
	return metadata, tree, nil
}

func (runner ManagerRunner) openManagerGuest(
	ctx context.Context,
	operation ManagerOperation,
	request vm.ConnectRequest,
) (vm.Session, func(context.Context) error, error) {
	if operation != ManagerResolve {
		session, err := runner.Connector.Connect(ctx, request)
		if err != nil {
			return nil, nil, err
		}
		return session, session.Close, nil
	}
	connector, ok := runner.Connector.(vm.PreparedConnector)
	if !ok {
		return nil, nil, errors.New(
			"dependency manager connector cannot prepare resolve guests",
		)
	}
	prepared, err := connector.Prepare(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	token, endpoint, err := prepareResolveRelay(ctx, prepared, runner.random)
	if err != nil {
		return nil, nil, errors.Join(err, closePreparedGuest(prepared))
	}
	session, err := prepared.Open(ctx)
	if err != nil {
		clearRelayCapability(&token)
		return nil, nil, errors.Join(
			err,
			endpoint.Close(),
			closePreparedGuest(prepared),
		)
	}
	closePrepared := func(ctx context.Context) error {
		clearRelayCapability(&token)
		return errors.Join(endpoint.Close(), prepared.Close(ctx))
	}
	return session, closePrepared, nil
}

func closePreparedGuest(prepared vm.PreparedSession) error {
	closeCtx, cancel := context.WithTimeout(
		context.Background(),
		managerRunCloseTimeout,
	)
	defer cancel()
	return prepared.Close(closeCtx)
}

func managerRunDrives(
	request ManagerRequest,
	drives ManagerRunDrives,
) ([]vm.ReadOnlyDrive, error) {
	required := []managerRunDrive{
		{vm.ManagerDrive, "manager", drives.Manager},
		{vm.ManagedRuntimeDrive, "managed runtime", drives.Runtime},
		{vm.ToolchainDrive, "standard toolchain", drives.StandardToolchain},
	}
	switch request.Operation {
	case ManagerProbe:
		if drives.Project != nil || drives.OfflineStore != nil {
			return nil, errors.New(
				"manager probe drives forbid project and offline store",
			)
		}
	case ManagerResolve:
		required = append(required, managerRunDrive{
			vm.ProjectDrive,
			"project",
			drives.Project,
		})
		if drives.OfflineStore != nil {
			return nil, errors.New("manager resolve drives forbid offline store")
		}
	case ManagerLifecycle:
		required = append(
			required,
			managerRunDrive{vm.ProjectDrive, "project", drives.Project},
			managerRunDrive{
				vm.OfflineStoreDrive,
				"offline store",
				drives.OfflineStore,
			},
		)
	default:
		return nil, fmt.Errorf("manager operation %q is unsupported", request.Operation)
	}

	out := make([]vm.ReadOnlyDrive, 0, len(required))
	for _, drive := range required {
		if drive.source == nil {
			return nil, fmt.Errorf("manager run %s drive is required", drive.label)
		}
		out = append(out, vm.ReadOnlyDrive{ID: drive.id, Source: drive.source})
	}
	return out, nil
}
