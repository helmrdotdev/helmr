ALTER TABLE runs
    DROP COLUMN IF EXISTS workspace_ref_kind,
    DROP COLUMN IF EXISTS workspace_ref_name,
    DROP COLUMN IF EXISTS workspace_full_ref,
    DROP COLUMN IF EXISTS workspace_default_branch,
    DROP COLUMN IF EXISTS workspace_pr_number,
    DROP COLUMN IF EXISTS workspace_pr_base_ref,
    DROP COLUMN IF EXISTS workspace_pr_base_sha,
    DROP COLUMN IF EXISTS workspace_pr_head_ref,
    DROP COLUMN IF EXISTS workspace_pr_head_sha;
