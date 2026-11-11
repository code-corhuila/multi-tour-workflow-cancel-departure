-- State of the cancel-departure saga (ADR-005 E4, ADR-004 D3). No foreign key here: it is in 04_alter.
-- tenant_id is a reference to identity-audit, never a foreign key (Annex J, J.3.4); departure_code and departure_id are references to
-- operations-cost, and the reservations are those of commercial-reservation. Nothing in this saga is undone, so there is no compensation
-- column: COMPENSATED only says that the cancellation was refused and nothing changed.
-- departure_id and cancelled_at are the answer of step 1, saved together at once: the 24 hours of the customer count from the instant
-- of the cancellation, so a saga resumed later must send the same one. version is raised by every save, so a save from an older copy of
-- a saga is refused and the claim of a stale saga is exclusive.
CREATE TABLE IF NOT EXISTS workflow_cancel_departure.cancel_departure_saga (
    saga_id         uuid CONSTRAINT pk_cancel_departure_saga PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text        NOT NULL,
    tenant_id       uuid        NOT NULL,
    departure_code  text        NOT NULL,
    departure_id    uuid,
    cancelled_at    timestamptz,
    saga_status     text        NOT NULL DEFAULT 'RUNNING',
    failed_step     text,
    failure_reason  text,
    version         bigint      NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_cancel_departure_saga_idempotency_key UNIQUE (tenant_id, idempotency_key),
    CONSTRAINT chk_cancel_departure_saga_idempotency_key CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
    CONSTRAINT chk_cancel_departure_saga_departure_code CHECK (char_length(departure_code) BETWEEN 1 AND 40),
    CONSTRAINT chk_cancel_departure_saga_cancellation CHECK ((departure_id IS NULL) = (cancelled_at IS NULL)),
    CONSTRAINT chk_cancel_departure_saga_status CHECK (saga_status IN ('RUNNING', 'COMPLETED', 'COMPENSATED', 'FAILED')),
    CONSTRAINT chk_cancel_departure_saga_failed_step CHECK (failed_step IN ('CANCEL_DEPARTURE', 'VOID_PROPOSALS', 'OPEN_EPISODES')),
    CONSTRAINT chk_cancel_departure_saga_failure_reason CHECK (failure_reason IN ('DEPARTURE_REFUSED', 'RESERVATIONS_FAILED', 'PARTICIPANT_UNAVAILABLE')),
    -- a saga that ended refused or failed says which step failed and why; the others have neither
    CONSTRAINT chk_cancel_departure_saga_failure CHECK ((saga_status IN ('COMPENSATED', 'FAILED')) = (failed_step IS NOT NULL)),
    CONSTRAINT chk_cancel_departure_saga_failure_pair CHECK ((failed_step IS NULL) = (failure_reason IS NULL)),
    -- nothing is undone: a saga is COMPENSATED only when step 1 was refused, and then nothing was touched
    CONSTRAINT chk_cancel_departure_saga_compensated CHECK (saga_status <> 'COMPENSATED' OR (failed_step = 'CANCEL_DEPARTURE' AND failure_reason = 'DEPARTURE_REFUSED')),
    -- a COMPLETED saga passed step 1, so it has the id and the instant that a resumption sends again; one refused at step 1 has neither
    CONSTRAINT chk_cancel_departure_saga_completed CHECK (saga_status <> 'COMPLETED' OR departure_id IS NOT NULL),
    CONSTRAINT chk_cancel_departure_saga_refused_without_departure CHECK (saga_status <> 'COMPENSATED' OR departure_id IS NULL)
);

CREATE TABLE IF NOT EXISTS workflow_cancel_departure.cancel_departure_saga_step (
    saga_id      uuid        NOT NULL,
    step_number  smallint    NOT NULL,
    step_name    text        NOT NULL,
    completed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_cancel_departure_saga_step PRIMARY KEY (saga_id, step_number),
    CONSTRAINT chk_cancel_departure_saga_step_number CHECK (step_number BETWEEN 1 AND 3),
    CONSTRAINT chk_cancel_departure_saga_step_name CHECK (step_name IN ('CANCEL_DEPARTURE', 'VOID_PROPOSALS', 'OPEN_EPISODES')),
    -- step 1 cancels the departure, step 2 voids the proposals and step 3 opens the episodes: the number and the name go together
    CONSTRAINT chk_cancel_departure_saga_step_order CHECK (
        (step_number = 1 AND step_name = 'CANCEL_DEPARTURE') OR (step_number = 2 AND step_name = 'VOID_PROPOSALS') OR (step_number = 3 AND step_name = 'OPEN_EPISODES'))
);

-- The reservations of the departure, listed in full before the first episode is opened and saved as each one is handled; a restart
-- goes on with the ones that are still PENDING. Only ids: no name and no text of a participant.
CREATE TABLE IF NOT EXISTS workflow_cancel_departure.cancel_departure_saga_reservation (
    saga_id           uuid        NOT NULL,
    reservation_id    uuid        NOT NULL,
    reservation_state text        NOT NULL DEFAULT 'PENDING',
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_cancel_departure_saga_reservation PRIMARY KEY (saga_id, reservation_id),
    CONSTRAINT chk_cancel_departure_saga_reservation_state CHECK (reservation_state IN ('PENDING', 'OPENED', 'FAILED'))
);
