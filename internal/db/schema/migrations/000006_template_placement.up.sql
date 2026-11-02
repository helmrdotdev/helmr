-- placement is the logical name a Template's Computers run under. The name
-- never identifies a worker group. The default only fills existing rows while
-- the column is added; every new definition states its placement.
ALTER TABLE template_definitions
    ADD COLUMN placement text NOT NULL DEFAULT 'default'
        CHECK (placement ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$');

ALTER TABLE template_definitions ALTER COLUMN placement DROP DEFAULT;
