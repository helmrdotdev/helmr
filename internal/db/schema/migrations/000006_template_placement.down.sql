UPDATE image_specs
SET spec = spec #- '{template,placement}'
WHERE spec ? 'template';

ALTER TABLE template_definitions DROP COLUMN placement;
