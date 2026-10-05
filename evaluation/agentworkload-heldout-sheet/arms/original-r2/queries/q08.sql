SELECT department, COUNT(DISTINCT city) AS distinct_cities FROM expense_detail GROUP BY department ORDER BY department
