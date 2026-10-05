SELECT department, city, SUM(amount) AS total_amount, COUNT(receipt_no) AS receipt_count FROM expense_detail GROUP BY department, city HAVING COUNT(receipt_no) > 10 ORDER BY department, city
