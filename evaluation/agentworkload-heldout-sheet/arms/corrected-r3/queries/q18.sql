SELECT month, SUM(total_amount) AS total_amount FROM expense_summary GROUP BY month ORDER BY month
