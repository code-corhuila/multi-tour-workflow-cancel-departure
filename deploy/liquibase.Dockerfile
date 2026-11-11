# Liquibase 5 ships without JDBC drivers (checked with 5.0.4: "Cannot find database driver"), so the
# runner is the official image plus the PostgreSQL driver, both at exact versions.
FROM liquibase/liquibase:5.0.4
RUN lpm add postgresql@42.7.13 --global
