-- Stop writers and drain object_delete_jobs before rollback; dropping a nonempty
-- queue loses pending object deletion intents. See development-and-operations.
DROP TABLE object_delete_jobs;
