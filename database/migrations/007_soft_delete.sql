ALTER TABLE files ADD COLUMN deleted_at INTEGER DEFAULT NULL;
CREATE INDEX idx_files_deleted ON files(deleted_at);
