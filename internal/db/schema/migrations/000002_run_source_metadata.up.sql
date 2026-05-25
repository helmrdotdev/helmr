ALTER TABLE runs
    ADD COLUMN workspace_ref_kind TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_ref_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_full_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_default_branch TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_pr_number INTEGER,
    ADD COLUMN workspace_pr_base_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_pr_base_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_pr_head_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_pr_head_sha TEXT NOT NULL DEFAULT '';

ALTER TABLE runs ALTER COLUMN workspace_ref_kind DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_ref_name DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_full_ref DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_default_branch DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_pr_base_ref DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_pr_base_sha DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_pr_head_ref DROP DEFAULT;
ALTER TABLE runs ALTER COLUMN workspace_pr_head_sha DROP DEFAULT;
