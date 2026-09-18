
SELECT department, COUNT(DISTINCT city) AS distinct_city_count
FROM expense_detail
GROUP BY department;

