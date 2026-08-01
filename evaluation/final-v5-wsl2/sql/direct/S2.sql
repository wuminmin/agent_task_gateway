SELECT o.orderkey, l.linenumber, l.extendedprice FROM reporting.orders o JOIN reporting.lineitem l ON l.orderkey=o.orderkey WHERE o.orderkey <= $1 ORDER BY o.orderkey,l.linenumber;
