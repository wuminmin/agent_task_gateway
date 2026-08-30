select s_suppkey, s_name, s_address, s_phone, total_revenue
from supplier, (select l_suppkey as supplier_no, sum(l_extendedprice * (1 - l_discount)) as total_revenue from lineitem where l_shipdate >= date '1996-01-01' and l_shipdate < date '1996-01-01' + interval '3' month group by l_suppkey) as revenue0
where s_suppkey = supplier_no and total_revenue = (select max(total_revenue) from (select l_suppkey as supplier_no, sum(l_extendedprice * (1 - l_discount)) as total_revenue from lineitem where l_shipdate >= date '1996-01-01' and l_shipdate < date '1996-01-01' + interval '3' month group by l_suppkey) as revenue1)
order by s_suppkey
