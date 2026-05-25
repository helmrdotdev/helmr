package control

import (
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type workspaceSourceDBFields struct {
	RefKind       string
	RefName       string
	FullRef       string
	DefaultBranch string
	PRNumber      pgtype.Int4
	PRBaseRef     string
	PRBaseSHA     string
	PRHeadRef     string
	PRHeadSHA     string
}

func workspaceSourceDBFieldsFromAPI(source api.GitHubSource) workspaceSourceDBFields {
	fields := workspaceSourceDBFields{
		RefKind:       string(source.RefKind),
		RefName:       source.RefName,
		FullRef:       source.FullRef,
		DefaultBranch: source.DefaultBranch,
	}
	if source.PullRequest != nil {
		fields.PRNumber = pgtype.Int4{Int32: source.PullRequest.Number, Valid: true}
		fields.PRBaseRef = source.PullRequest.BaseRef
		fields.PRBaseSHA = source.PullRequest.BaseSHA
		fields.PRHeadRef = source.PullRequest.HeadRef
		fields.PRHeadSHA = source.PullRequest.HeadSHA
	}
	return fields
}

func githubSourceFromRun(row db.Run) api.GitHubSource {
	return githubSourceFromFields(
		row.WorkspaceRepository,
		row.WorkspaceRef,
		row.WorkspaceSha,
		row.WorkspaceSubpath,
		row.WorkspaceRefKind,
		row.WorkspaceRefName,
		row.WorkspaceFullRef,
		row.WorkspaceDefaultBranch,
		row.WorkspacePrNumber,
		row.WorkspacePrBaseRef,
		row.WorkspacePrBaseSha,
		row.WorkspacePrHeadRef,
		row.WorkspacePrHeadSha,
	)
}

func githubSourceFromLeaseRow(row db.LeaseRunExecutionRow) api.GitHubSource {
	return githubSourceFromFields(
		row.WorkspaceRepository,
		row.WorkspaceRef,
		row.WorkspaceSha,
		row.WorkspaceSubpath,
		row.WorkspaceRefKind,
		row.WorkspaceRefName,
		row.WorkspaceFullRef,
		row.WorkspaceDefaultBranch,
		row.WorkspacePrNumber,
		row.WorkspacePrBaseRef,
		row.WorkspacePrBaseSha,
		row.WorkspacePrHeadRef,
		row.WorkspacePrHeadSha,
	)
}

func githubSourceFromFields(
	repository string,
	ref string,
	sha string,
	subpath string,
	refKind string,
	refName string,
	fullRef string,
	defaultBranch string,
	prNumber pgtype.Int4,
	prBaseRef string,
	prBaseSHA string,
	prHeadRef string,
	prHeadSHA string,
) api.GitHubSource {
	source := api.GitHubSource{
		Repository:    repository,
		Ref:           ref,
		SHA:           sha,
		Subpath:       subpath,
		RefKind:       api.GitHubRefKind(refKind),
		RefName:       refName,
		FullRef:       fullRef,
		DefaultBranch: defaultBranch,
	}
	if prNumber.Valid {
		source.PullRequest = &api.GitHubPullRequestMetadata{
			Number:  prNumber.Int32,
			BaseRef: prBaseRef,
			BaseSHA: prBaseSHA,
			HeadRef: prHeadRef,
			HeadSHA: prHeadSHA,
		}
	}
	return source
}
