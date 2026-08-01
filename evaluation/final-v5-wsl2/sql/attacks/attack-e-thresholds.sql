-- Preregistered sequence; do not modify after observing a bounded run.
SELECT COUNT(*) FROM reporting.orders WHERE orderkey > 1000;
SELECT COUNT(*) FROM reporting.orders WHERE orderkey > 2000;
SELECT COUNT(*) FROM reporting.orders WHERE orderkey > 3000;
SELECT COUNT(*) FROM reporting.orders WHERE orderkey > 4000;
SELECT COUNT(*) FROM reporting.orders WHERE orderkey > 5000;
