SELECT city, COUNT(DISTINCT employee_no) AS distinct_employees FROM expense_detail GROUP BY city ORDER BY city
