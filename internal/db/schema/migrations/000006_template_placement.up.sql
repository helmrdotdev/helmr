-- placement is the logical name a Template's Computers run under. Each
-- environment binds the name to capacity in environment_placements; the name
-- never identifies a worker group. Definitions recorded before placements
-- existed run on the default placement.
ALTER TABLE template_definitions
    ADD COLUMN placement text NOT NULL DEFAULT 'default'
        CHECK (placement ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$');

ALTER TABLE template_definitions ALTER COLUMN placement DROP DEFAULT;
