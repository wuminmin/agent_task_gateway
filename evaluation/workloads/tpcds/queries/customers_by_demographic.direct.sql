SELECT cd_gender,
       cd_marital_status,
       count(cd_demo_sk) AS demographic_count
FROM public.customer_demographics
GROUP BY cd_gender, cd_marital_status
ORDER BY cd_gender, cd_marital_status
