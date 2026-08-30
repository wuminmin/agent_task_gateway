select c_nation, s_nation, d_year, sum(lo_revenue) as revenue
from lineorder join customer on lo_custkey = c_custkey join supplier on lo_suppkey = s_suppkey join dwdate on lo_orderdate = d_datekey
where c_region = 'ASIA' and s_region = 'ASIA' and d_year >= 1992 and d_year <= 1997
group by c_nation, s_nation, d_year
order by d_year asc, revenue desc;
