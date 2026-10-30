CREATE TABLE `session_deletions` (
	`session_id` text PRIMARY KEY NOT NULL,
	`operation_id` text NOT NULL,
	`native_session_id` text,
	`workspace_path` text,
	`turn_count` integer NOT NULL,
	`stage` text NOT NULL
);
