select d_year, c_nation, sum(lo_revenue - lo_supplycost) as profit
from lineorder join customer on lo_custkey = c_custkey join supplier on lo_suppkey = s_suppkey join part on lo_partkey = p_partkey join dwdate on lo_orderdate = d_datekey
where c_region = 'AMERICA' and s_region = 'AMERICA' and (p_mfgr = 'MFGR#1' or p_mfgr = 'MFGR#2')
group by d_year, c_nation
order by d_year, c_nation;
