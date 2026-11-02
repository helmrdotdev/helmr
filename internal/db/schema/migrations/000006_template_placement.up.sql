-- placement is the logical name a Template's Computers run under. The name
-- never identifies a worker group. Definitions recorded before placements
-- existed use the default placement.
ALTER TABLE template_definitions
    ADD COLUMN placement text NOT NULL DEFAULT 'default'
        CHECK (placement ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$');

ALTER TABLE template_definitions ALTER COLUMN placement DROP DEFAULT;

-- A preparation spec for a Template without a callback embeds the Template,
-- which now always states its placement. spec_digest stays the identity under
-- which the spec was registered; a later deployment of the same Template
-- registers a new spec under the digest of its current form.
UPDATE image_specs
SET spec = jsonb_set(spec, '{template,placement}', '"default"')
WHERE spec ? 'template';
