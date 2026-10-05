SELECT department, city, SUM(amount) AS total_amount, COUNT(*) AS receipt_count FROM expense_detail GROUP BY department, city HAVING COUNT(*) > 10 ORDER BY department, city
