-- Track the last successful deployment separately from the (possibly failed)
-- desired one, so a failed redeploy can roll back to something known-good
-- instead of only ever marking the environment failed.
ALTER TABLE environments ADD COLUMN last_good_artifact_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN last_good_deploy_ref TEXT NOT NULL DEFAULT '';
