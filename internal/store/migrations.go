package store

// migrations are applied in order and tracked with PRAGMA user_version.
// Never edit a released migration; append a new one.
var migrations = []string{
	// 1: control plane basics
	`
CREATE TABLE apps (
	id                 INTEGER PRIMARY KEY,
	name               TEXT    NOT NULL UNIQUE,
	locked             INTEGER NOT NULL DEFAULT 0,
	current_release_id INTEGER,
	created_at         INTEGER NOT NULL
);

-- app_id 0 is the global scope for config_vars, domains and properties.
CREATE TABLE config_vars (
	app_id INTEGER NOT NULL,
	key    TEXT    NOT NULL,
	value  TEXT    NOT NULL,
	PRIMARY KEY (app_id, key)
);

CREATE TABLE domains (
	app_id INTEGER NOT NULL,
	domain TEXT    NOT NULL,
	PRIMARY KEY (app_id, domain)
);
CREATE UNIQUE INDEX domains_unique_app_domain ON domains (domain) WHERE app_id != 0;

CREATE TABLE properties (
	app_id INTEGER NOT NULL,
	plugin TEXT    NOT NULL,
	key    TEXT    NOT NULL,
	value  TEXT    NOT NULL,
	PRIMARY KEY (app_id, plugin, key)
);

CREATE TABLE formations (
	app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
	process_type TEXT    NOT NULL,
	quantity     INTEGER NOT NULL,
	PRIMARY KEY (app_id, process_type)
);

-- process_type '' is the app-wide default size.
CREATE TABLE resources (
	app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
	process_type TEXT    NOT NULL,
	cpus         INTEGER NOT NULL DEFAULT 0,
	memory_mb    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (app_id, process_type)
);

CREATE TABLE releases (
	id           INTEGER PRIMARY KEY,
	app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
	version      INTEGER NOT NULL,
	artifact     TEXT    NOT NULL DEFAULT '',
	processes    TEXT    NOT NULL DEFAULT '{}',
	image_config TEXT    NOT NULL DEFAULT '{}',
	config_vars  TEXT    NOT NULL DEFAULT '{}',
	description  TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	UNIQUE (app_id, version)
);

CREATE TABLE deploys (
	id          INTEGER PRIMARY KEY,
	app_id      INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
	release_id  INTEGER,
	source      TEXT    NOT NULL,
	source_ref  TEXT    NOT NULL DEFAULT '',
	status      TEXT    NOT NULL,
	error       TEXT    NOT NULL DEFAULT '',
	actor       TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	finished_at INTEGER
);
CREATE INDEX deploys_app ON deploys (app_id, id);

CREATE TABLE ssh_keys (
	name        TEXT    PRIMARY KEY,
	fingerprint TEXT    NOT NULL UNIQUE,
	public_key  TEXT    NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE nodes (
	id           INTEGER PRIMARY KEY,
	name         TEXT    NOT NULL UNIQUE,
	role         TEXT    NOT NULL,
	subnet_index INTEGER NOT NULL UNIQUE,
	address      TEXT    NOT NULL DEFAULT '',
	arch         TEXT    NOT NULL DEFAULT '',
	cpus         INTEGER NOT NULL DEFAULT 0,
	memory_mb    INTEGER NOT NULL DEFAULT 0,
	wg_public_key TEXT   NOT NULL DEFAULT '',
	wg_endpoint  TEXT    NOT NULL DEFAULT '',
	token_hash   TEXT    NOT NULL DEFAULT '',
	schedulable  INTEGER NOT NULL DEFAULT 1,
	ingress      INTEGER NOT NULL DEFAULT 1,
	draining     INTEGER NOT NULL DEFAULT 0,
	last_seen    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL
);
`,

	// 2: running instances (microVMs) and stopped apps
	`
CREATE TABLE instances (
	id           TEXT    PRIMARY KEY,
	app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
	release_id   INTEGER NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
	process_type TEXT    NOT NULL,
	idx          INTEGER NOT NULL,
	node         TEXT    NOT NULL,
	ip           TEXT    NOT NULL UNIQUE,
	port         INTEGER NOT NULL,
	cpus         INTEGER NOT NULL,
	memory_mb    INTEGER NOT NULL,
	desired      TEXT    NOT NULL,
	state        TEXT    NOT NULL DEFAULT 'pending',
	healthy_once INTEGER NOT NULL DEFAULT 0,
	restarts     INTEGER NOT NULL DEFAULT 0,
	retire_at    INTEGER,
	started_at   INTEGER,
	created_at   INTEGER NOT NULL
);
CREATE INDEX instances_app ON instances (app_id);

ALTER TABLE apps ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;
`,

	// 3: clusters: node credentials and metrics, join tokens, events, artifact
	// checksums for distribution, observed instance usage, shared TLS storage
	`
ALTER TABLE nodes ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN agent_token TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN can_run TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN metrics TEXT NOT NULL DEFAULT '{}';

ALTER TABLE releases ADD COLUMN artifact_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN artifact_size INTEGER NOT NULL DEFAULT 0;

ALTER TABLE instances ADD COLUMN replaces TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN cpu_percent REAL NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN memory_used_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN reported_at INTEGER NOT NULL DEFAULT 0;

CREATE TABLE join_tokens (
	hash       TEXT    PRIMARY KEY,
	reusable   INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE events (
	id      INTEGER PRIMARY KEY,
	at      INTEGER NOT NULL,
	kind    TEXT    NOT NULL,
	app     TEXT    NOT NULL DEFAULT '',
	node    TEXT    NOT NULL DEFAULT '',
	message TEXT    NOT NULL
);

CREATE TABLE certstore (
	key      TEXT    PRIMARY KEY,
	value    BLOB    NOT NULL,
	modified INTEGER NOT NULL
);

CREATE TABLE certlocks (
	key        TEXT    PRIMARY KEY,
	owner      TEXT    NOT NULL,
	expires_at INTEGER NOT NULL
);
`,

	// 4: volumes. A destroyed volume's row stays (state 'destroying', and
	// app_id NULL once its app is gone) until its node has deleted the disk.
	// Move columns describe a copy to moving_to in progress; previous_node
	// keeps the old copy until the new node reports the disk.
	`
CREATE TABLE volumes (
	id            TEXT    PRIMARY KEY,
	app_id        INTEGER REFERENCES apps (id) ON DELETE SET NULL,
	name          TEXT    NOT NULL,
	type          TEXT    NOT NULL,
	size_mb       INTEGER NOT NULL,
	state         TEXT    NOT NULL DEFAULT 'new',
	node          TEXT    NOT NULL DEFAULT '',
	status        TEXT    NOT NULL DEFAULT '',
	used_mb       INTEGER NOT NULL DEFAULT 0,
	previous_node TEXT    NOT NULL DEFAULT '',
	moving_to     TEXT    NOT NULL DEFAULT '',
	move_token    TEXT    NOT NULL DEFAULT '',
	transfer      TEXT    NOT NULL DEFAULT '',
	copied_mb     INTEGER NOT NULL DEFAULT 0,
	move_error    TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX volumes_app_name ON volumes (app_id, name) WHERE state != 'destroying';

CREATE TABLE volume_mounts (
	volume_id    TEXT NOT NULL REFERENCES volumes (id) ON DELETE CASCADE,
	process_type TEXT NOT NULL,
	path         TEXT NOT NULL,
	PRIMARY KEY (volume_id, process_type, path)
);

ALTER TABLE instances ADD COLUMN volumes TEXT NOT NULL DEFAULT '[]';
ALTER TABLE nodes ADD COLUMN features TEXT NOT NULL DEFAULT '';
`,

	// 5: credentials for pulling images from private registries
	`
CREATE TABLE registry_logins (
	server     TEXT    PRIMARY KEY,
	username   TEXT    NOT NULL,
	password   TEXT    NOT NULL,
	created_at INTEGER NOT NULL
);
`,

	// 6: compose releases: an image per process type, and the process that
	// gets HTTP traffic
	`
ALTER TABLE releases ADD COLUMN services TEXT NOT NULL DEFAULT '{}';
ALTER TABLE releases ADD COLUMN web TEXT NOT NULL DEFAULT '';
`,

	// 7: how an app is built is one set of builder properties: type (was
	// builder selected), file (was builder-dockerfile dockerfile-path or
	// builder-compose compose-file), image, build-dir and procfile (was ps
	// procfile-path). Apps whose last deploy was an image become image apps.
	`
UPDATE properties SET key = 'type' WHERE plugin = 'builder' AND key = 'selected';
DELETE FROM properties WHERE plugin = 'builder' AND key = 'type' AND app_id = 0;
INSERT OR REPLACE INTO properties (app_id, plugin, key, value)
	SELECT app_id, 'builder', 'file', value FROM properties
	WHERE app_id != 0 AND plugin = 'builder-dockerfile' AND key = 'dockerfile-path'
		AND app_id NOT IN (SELECT app_id FROM properties WHERE plugin = 'builder' AND key = 'type' AND value = 'compose');
INSERT OR REPLACE INTO properties (app_id, plugin, key, value)
	SELECT app_id, 'builder', 'file', value FROM properties
	WHERE app_id != 0 AND plugin = 'builder-compose' AND key = 'compose-file'
		AND app_id IN (SELECT app_id FROM properties WHERE plugin = 'builder' AND key = 'type' AND value = 'compose');
DELETE FROM properties WHERE plugin IN ('builder-dockerfile', 'builder-compose');
UPDATE properties SET plugin = 'builder', key = 'procfile' WHERE plugin = 'ps' AND key = 'procfile-path';

CREATE TEMP TABLE image_apps AS
	SELECT d.app_id, d.source_ref FROM deploys d
	WHERE d.source = 'image' AND d.id = (SELECT MAX(id) FROM deploys WHERE app_id = d.app_id AND status = 'succeeded');
DELETE FROM properties WHERE plugin = 'builder' AND key = 'file' AND app_id IN (SELECT app_id FROM image_apps);
INSERT OR REPLACE INTO properties (app_id, plugin, key, value) SELECT app_id, 'builder', 'type', 'image' FROM image_apps;
INSERT OR REPLACE INTO properties (app_id, plugin, key, value) SELECT app_id, 'builder', 'image', source_ref FROM image_apps;
DROP TABLE image_apps;
`,
}
