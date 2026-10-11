package db

// RevokedComputerSecretsSQL derives revocation from all durable exposure paths.
// Revocation follows the pinned image across disk saves and RAM checkpoints.
// Historical processes and commands remain relevant after they stop. Keep the
// static copy in query/allocation_planning.sql identical for sqlc.
const RevokedComputerSecretsSQL = `SELECT c.environment_id,c.id AS computer_id,x.secret_id
    FROM computers c JOIN computer_images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id)
    JOIN secret_exposures x ON (x.environment_id,x.preparation_id)=(i.environment_id,i.preparation_id)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
    WHERE s.status='revoked'
    UNION
    SELECT p.environment_id,p.computer_id,x.secret_id FROM secret_exposures x
    JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(x.environment_id,x.session_id,x.process_epoch)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE s.status='revoked'
    UNION
    SELECT c.environment_id,c.computer_id,x.secret_id FROM secret_exposures x
    JOIN computer_commands c ON (c.environment_id,c.id)=(x.environment_id,x.command_id)
    JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id) WHERE s.status='revoked'`
