select s_acctbal, s_name, n_name, p_partkey, p_mfgr, s_address, s_phone, s_comment
from part join partsupp on p_partkey = ps_partkey join supplier on s_suppkey = ps_suppkey join nation on s_nationkey = n_nationkey join region on n_regionkey = r_regionkey
where p_size = 15 and p_type like '%BRASS' and r_name = 'EUROPE'
and ps_supplycost = (select min(ps_supplycost) from partsupp join supplier on s_suppkey = ps_suppkey join nation on s_nationkey = n_nationkey join region on n_regionkey = r_regionkey where p_partkey = ps_partkey and r_name = 'EUROPE')
order by s_acctbal desc, n_name, s_name, p_partkey
limit 100
