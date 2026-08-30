select d_year, s_city, p_brand1, sum(lo_revenue - lo_supplycost) as profit
from lineorder join customer on lo_custkey = c_custkey join supplier on lo_suppkey = s_suppkey join part on lo_partkey = p_partkey join dwdate on lo_orderdate = d_datekey
where c_region = 'AMERICA' and s_nation = 'UNITED STATES' and (d_year = 1997 or d_year = 1998) and p_category = 'MFGR#14'
group by d_year, s_city, p_brand1
order by d_year, s_city, p_brand1;
