ALTER TABLE documents
    ADD COLUMN kind VARCHAR(32) NOT NULL DEFAULT 'document';

ALTER TABLE rbac_roles
    ADD COLUMN prompt TEXT;

DELETE FROM rbac_menus
WHERE path IN ('/rbac/users', '/rbac/roles', '/rbac/menus', '/rbac/keys');
