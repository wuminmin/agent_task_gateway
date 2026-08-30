SELECT SUM(extendedprice) AS total_extended_price
FROM provsql_lineitem
WHERE orderkey <= 1000;
