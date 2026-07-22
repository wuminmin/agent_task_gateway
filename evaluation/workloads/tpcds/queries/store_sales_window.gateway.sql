SELECT ss_store_sk,
       count(ss_item_sk) AS sale_count,
       sum(ss_quantity) AS total_quantity,
       sum(ss_net_paid) AS total_net_paid
FROM tpcds_store_sales
WHERE ss_sold_date_sk >= 2450815
  AND ss_sold_date_sk < 2451180
GROUP BY ss_store_sk
ORDER BY ss_store_sk
