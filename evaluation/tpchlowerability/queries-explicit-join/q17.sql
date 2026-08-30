select sum(l_extendedprice) / 7.0 as avg_yearly
from lineitem join part on p_partkey = l_partkey
where p_brand = 'Brand#23' and p_container = 'MED BOX'
and l_quantity < (select 0.2 * avg(l_quantity) from lineitem where l_partkey = p_partkey)
