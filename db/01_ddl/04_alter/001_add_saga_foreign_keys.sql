-- The index of each key is the primary key of its table, whose first column is saga_id (Annex A).
-- Guarded, so applying the changeset again does nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_cancel_departure_saga_step_saga') THEN
        ALTER TABLE workflow_cancel_departure.cancel_departure_saga_step
            ADD CONSTRAINT fk_cancel_departure_saga_step_saga FOREIGN KEY (saga_id) REFERENCES workflow_cancel_departure.cancel_departure_saga (saga_id) ON DELETE CASCADE;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_cancel_departure_saga_reservation_saga') THEN
        ALTER TABLE workflow_cancel_departure.cancel_departure_saga_reservation
            ADD CONSTRAINT fk_cancel_departure_saga_reservation_saga FOREIGN KEY (saga_id) REFERENCES workflow_cancel_departure.cancel_departure_saga (saga_id) ON DELETE CASCADE;
    END IF;
END
$$;
