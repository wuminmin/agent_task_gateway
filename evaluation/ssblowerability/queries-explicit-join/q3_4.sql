select c_city, s_city, d_year, sum(lo_revenue) as revenue
from lineorder join customer on lo_custkey = c_custkey join supplier on lo_suppkey = s_suppkey join dwdate on lo_orderdate = d_datekey
where (c_city = 'UNITED KI1' or c_city = 'UNITED KI5') and (s_city = 'UNITED KI1' or s_city = 'UNITED KI5') and d_yearmonth = 'Dec1997'
group by c_city, s_city, d_year
order by d_year asc, revenue desc;
