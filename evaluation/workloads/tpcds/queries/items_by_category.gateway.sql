SELECT i_category,
       count(i_item_sk) AS item_count,
       avg(i_current_price) AS average_price
FROM tpcds_item
GROUP BY i_category
ORDER BY i_category
