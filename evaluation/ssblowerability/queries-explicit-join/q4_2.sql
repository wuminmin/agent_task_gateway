select d_year, s_nation, p_category, sum(lo_revenue - lo_supplycost) as profit
from lineorder join customer on lo_custkey = c_custkey join supplier on lo_suppkey = s_suppkey join part on lo_partkey = p_partkey join dwdate on lo_orderdate = d_datekey
where c_region = 'AMERICA' and s_region = 'AMERICA' and (d_year = 1997 or d_year = 1998) and (p_mfgr = 'MFGR#1' or p_mfgr = 'MFGR#2')
group by d_year, s_nation, p_category
order by d_year, s_nation, p_category;
