select sum(lo_revenue), d_year, p_brand1
from lineorder join dwdate on lo_orderdate = d_datekey join part on lo_partkey = p_partkey join supplier on lo_suppkey = s_suppkey
where p_brand1 = 'MFGR#2239' and s_region = 'EUROPE'
group by d_year, p_brand1
order by d_year, p_brand1;
