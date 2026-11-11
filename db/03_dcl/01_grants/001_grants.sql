GRANT USAGE ON SCHEMA workflow_cancel_departure TO workflow_cancel_departure_reader, workflow_cancel_departure_writer;
GRANT SELECT ON ALL TABLES IN SCHEMA workflow_cancel_departure TO workflow_cancel_departure_reader;
-- No DELETE: a saga is never removed by the service, the state is the audit of the process
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA workflow_cancel_departure TO workflow_cancel_departure_writer;
ALTER DEFAULT PRIVILEGES IN SCHEMA workflow_cancel_departure GRANT SELECT ON TABLES TO workflow_cancel_departure_reader;
ALTER DEFAULT PRIVILEGES IN SCHEMA workflow_cancel_departure GRANT SELECT, INSERT, UPDATE ON TABLES TO workflow_cancel_departure_writer;
GRANT workflow_cancel_departure_writer TO workflow_cancel_departure_app;
